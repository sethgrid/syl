package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/sethgrid/syl/internal/agent"
	"github.com/sethgrid/syl/internal/chat"
	"github.com/sethgrid/syl/internal/classifier"
	"github.com/sethgrid/syl/internal/claude"
	"github.com/sethgrid/syl/internal/db"
	"github.com/sethgrid/syl/internal/inbox"
	"github.com/sethgrid/syl/internal/jobs"
	"github.com/sethgrid/syl/internal/skills"
	"github.com/sethgrid/syl/internal/sse"
	"github.com/sethgrid/syl/logger"
)

// Server is the Syl HTTP server.
type Server struct {
	config               Config
	agents               agent.Store
	chats                chat.Store
	inboxItems           inbox.Store
	jobStore             jobs.Store
	broker               *sse.Broker
	clf                  classifier.Classifier
	claude               claude.Client
	skills               *skills.Loader
	compactionThreshold  int

	mu                 sync.Mutex
	started            bool
	port               int
	internalPort       int
	srvErr             error
	publicHTTPServer   *http.Server
	internalHTTPServer *http.Server
	jobRunner          *jobs.Runner
	parentLogger       *slog.Logger
}

// New creates a production server wired to real SQLite and Claude.
func New(conf Config) (*Server, error) {
	rootLogger := logger.New().With("version", conf.Version)

	sqlDB, err := db.Open(conf.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	claudeClient := claude.New(conf.AnthropicAPIKey)

	return &Server{
		config:              conf,
		port:                conf.Port,
		agents:              agent.NewSQLiteStore(sqlDB),
		chats:               chat.NewSQLiteStore(sqlDB),
		inboxItems:          inbox.NewSQLiteStore(sqlDB),
		jobStore:            jobs.NewSQLiteStore(sqlDB),
		broker:              sse.NewBroker(sqlDB),
		clf:                 classifier.NewClaudeClassifier(newMetricsClient(claudeClient, "classifier"), time.Now),
		claude:              newMetricsClient(claudeClient, "response"),
		skills:              skills.NewLoader(conf.SkillsDir, conf.Debug),
		compactionThreshold: conf.CompactionThreshold,
		parentLogger:        rootLogger,
	}, nil
}

// Option is a functional option for test setup.
type Option func(*Server)

func WithAgents(s agent.Store) Option                  { return func(srv *Server) { srv.agents = s } }
func WithChats(s chat.Store) Option                    { return func(srv *Server) { srv.chats = s } }
func WithInbox(s inbox.Store) Option                   { return func(srv *Server) { srv.inboxItems = s } }
func WithJobs(s jobs.Store) Option                     { return func(srv *Server) { srv.jobStore = s } }
func WithClaude(c claude.Client) Option                { return func(srv *Server) { srv.claude = c } }
func WithClassifier(c classifier.Classifier) Option    { return func(srv *Server) { srv.clf = c } }
func WithLogger(l *slog.Logger) Option                 { return func(srv *Server) { srv.parentLogger = l } }

// NewTest creates a server wired with in-memory fakes for testing.
func NewTest(conf Config, opts ...Option) *Server {
	srv := &Server{
		config:              conf,
		port:                conf.Port,
		agents:              agent.NewFakeStore(),
		chats:               chat.NewFakeStore(),
		inboxItems:          inbox.NewFakeStore(),
		jobStore:            jobs.NewFakeStore(),
		broker:              sse.NewBroker(nil),
		clf:                 &classifier.FakeClassifier{},
		claude:              &claude.FakeClient{},
		skills:              skills.NewLoader("", false),
		compactionThreshold: conf.CompactionThreshold,
		parentLogger:        logger.New(),
	}
	for _, opt := range opts {
		opt(srv)
	}
	return srv
}

func (s *Server) Serve() error {
	privateRouter := chi.NewRouter()
	privateRouter.Handle("/metrics", promhttp.Handler())
	privateRouter.Get("/healthcheck", handleHealthcheck())
	privateRouter.Get("/status", handleStatus(s.config.Version))

	router := s.newRouter()

	withTimeout := middleware.Timeout(s.config.RequestTimeout)

	router.With(withTimeout).Get("/", handleIndex())
	router.With(withTimeout).Get("/session", handleSession(s.agents))
	router.With(withTimeout).Get("/history", handleHistory(s.chats))
	// SSE must NOT have a request timeout
	router.Get("/sse", handleSSE(s.broker, sseActiveConns))
	router.With(withTimeout).Post("/message", handleMessage(
		s.agents, s.chats, s.broker, s.clf, s.claude, s.skills, s.inboxItems, s.jobStore, s.compactionThreshold,
	))
	router.With(withTimeout).Post("/chat", handleChat(
		s.agents, s.chats, s.claude, s.skills,
	))
	router.With(withTimeout).Get("/inbox", handleInboxList(s.inboxItems))
	router.With(withTimeout).Post("/inbox/{id}/answer", handleInboxAnswer(s.inboxItems))
	router.With(withTimeout).Get("/agents/{id}/soul", handleGetSoul(s.agents))
	router.With(withTimeout).Put("/agents/{id}/soul", handlePutSoul(s.agents))

	s.mu.Lock()
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		s.mu.Unlock()
		s.setLastError(err)
		return fmt.Errorf("listen: %w", err)
	}
	s.started = true
	s.port = listener.Addr().(*net.TCPAddr).Port
	s.parentLogger.Info("starting public listener", "port", s.port)
	s.mu.Unlock()

	go func() {
		internalHTTP := &http.Server{Handler: privateRouter}
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", s.config.InternalPort))
		if err != nil {
			s.parentLogger.Error("internal listener failed", "error", err)
			return
		}
		s.mu.Lock()
		s.internalHTTPServer = internalHTTP
		s.internalPort = l.Addr().(*net.TCPAddr).Port
		s.parentLogger.Info("starting internal listener", "port", s.internalPort)
		s.mu.Unlock()
		_ = internalHTTP.Serve(l)
	}()

	processor := &jobProcessor{
		claude:               s.claude,
		broker:               s.broker,
		agents:               s.agents,
		chats:                s.chats,
		inboxItems:           s.inboxItems,
		skills:               s.skills,
		jobStore:             s.jobStore,
		clf:                  s.clf,
		logger:               s.parentLogger,
		compactionThreshold:  s.compactionThreshold,
	}
	runner := jobs.NewRunner(s.jobStore, processor, s.parentLogger, 15*time.Second)
	s.mu.Lock()
	s.jobRunner = runner
	s.mu.Unlock()
	go runner.Start()

	publicHTTP := &http.Server{Handler: router}
	s.mu.Lock()
	s.publicHTTPServer = publicHTTP
	s.mu.Unlock()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		s.parentLogger.Info("shutdown signal received")
		_ = s.Close()
	}()

	if err := publicHTTP.Serve(listener); err != nil && err != http.ErrServerClosed {
		s.setLastError(err)
		return err
	}
	return nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	timeout := s.config.ShutdownTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var g errgroup.Group
	if s.publicHTTPServer != nil {
		g.Go(func() error { return s.publicHTTPServer.Shutdown(ctx) })
	}
	if s.internalHTTPServer != nil {
		g.Go(func() error { return s.internalHTTPServer.Shutdown(ctx) })
	}
	if s.jobRunner != nil {
		g.Go(func() error { return s.jobRunner.Close() })
	}
	return g.Wait()
}

func (s *Server) newRouter() *chi.Mux {
	router := chi.NewRouter()
	router.Use(middleware.RealIP)
	router.Use(logger.Middleware(s.parentLogger, s.config.Debug))
	router.Use(middleware.Recoverer)
	router.Use(httpMetricsMiddleware)
	return router
}

func (s *Server) Port() int {
	for i := 0; i < 10; i++ {
		s.mu.Lock()
		p := s.port
		s.mu.Unlock()
		if p != 0 {
			return p
		}
		time.Sleep(100 * time.Duration(i) * time.Millisecond)
	}
	return 0
}

func (s *Server) IsStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *Server) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srvErr
}

func (s *Server) setLastError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.srvErr = err
}
