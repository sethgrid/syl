package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Loader reads skill files from disk at startup.
// Skills are Markdown files in the skills directory.
// Dev skills (skills/dev/) are loaded only when debug=true.
type Loader struct {
	dir    string
	skills map[string]string // name → content
}

func NewLoader(dir string, debug bool) *Loader {
	l := &Loader{dir: dir, skills: make(map[string]string)}
	if dir == "" {
		return l
	}
	l.loadDir(dir)
	if debug {
		l.loadDir(filepath.Join(dir, "dev"))
	}
	return l
}

func (l *Loader) loadDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		l.skills[name] = string(data)
	}
}

// Names returns all loaded skill names.
func (l *Loader) Names() []string {
	names := make([]string, 0, len(l.skills))
	for n := range l.skills {
		names = append(names, n)
	}
	return names
}

// Get returns the content of a skill by name, or empty string if not found.
func (l *Loader) Get(name string) string {
	return l.skills[name]
}

// Write saves a skill to disk and updates the in-memory map.
// Returns an error if no directory was configured.
func (l *Loader) Write(name, content string) error {
	if l.dir == "" {
		return fmt.Errorf("skills: no directory configured")
	}
	path := filepath.Join(l.dir, name+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write skill %q: %w", name, err)
	}
	l.skills[name] = content
	return nil
}

// GetMany returns the content of multiple skills, joined with a separator.
func (l *Loader) GetMany(names []string) string {
	var sb strings.Builder
	for _, n := range names {
		if c := l.skills[n]; c != "" {
			sb.WriteString("## Skill: ")
			sb.WriteString(n)
			sb.WriteString("\n\n")
			sb.WriteString(c)
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}
