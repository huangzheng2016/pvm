// Package apps is the built-in catalog of programs `pvm app install` can
// put into a VM. Each app is a folder named by its id, with an app.yaml
// and an install.sh; nothing else can be installed through it.
package apps

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed prologue.sh */app.yaml */install.sh
var files embed.FS

// idRe is the daemon's app id rule.
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// App is one catalog entry.
type App struct {
	ID          string `yaml:"-" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	// Homepage is the vendor's download or documentation page.
	Homepage string `yaml:"homepage" json:"homepage"`
	// Source is official for the vendor's own build, else community.
	Source string `yaml:"source" json:"source"`
	// Window is true for an app that shows a window through Xpra.
	Window bool `yaml:"window" json:"window"`
	// Start is the command line that starts the app.
	Start string `yaml:"start" json:"start"`
	// Script installs the app. It runs as root under bash in the VM, with
	// PVM_USER set to the VM user, and leaves an installed app as it is.
	Script string `yaml:"-" json:"-"`
}

var catalog = sync.OnceValue(func() []App {
	list, err := load(files)
	if err != nil {
		panic("apps: " + err.Error())
	}
	return list
})

// ValidID reports whether id has the shape of an app id.
func ValidID(id string) bool { return idRe.MatchString(id) }

// List returns the catalog, sorted by id.
func List() []App { return catalog() }

func Get(id string) (App, bool) {
	for _, a := range catalog() {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// load reads every app folder of fsys; prologue.sh goes before each script.
func load(fsys fs.FS) ([]App, error) {
	prologue, err := fs.ReadFile(fsys, "prologue.sh")
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var list []App
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		a := App{ID: e.Name()}
		if !idRe.MatchString(a.ID) {
			return nil, fmt.Errorf("invalid app id %q: lowercase letters, digits and -, at most 32", a.ID)
		}
		raw, err := fs.ReadFile(fsys, a.ID+"/app.yaml")
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("%s/app.yaml: %w", a.ID, err)
		}
		for _, f := range []struct{ key, val string }{{"name", a.Name}, {"description", a.Description}, {"homepage", a.Homepage}, {"start", a.Start}} {
			if strings.TrimSpace(f.val) == "" {
				return nil, fmt.Errorf("%s/app.yaml: %s is empty", a.ID, f.key)
			}
		}
		if !strings.HasPrefix(a.Homepage, "https://") {
			return nil, fmt.Errorf("%s/app.yaml: homepage %q is not an https URL", a.ID, a.Homepage)
		}
		if a.Source != "official" && a.Source != "community" {
			return nil, fmt.Errorf("%s/app.yaml: source %q is not official or community", a.ID, a.Source)
		}
		script, err := fs.ReadFile(fsys, a.ID+"/install.sh")
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(script)) == 0 {
			return nil, fmt.Errorf("%s/install.sh is empty", a.ID)
		}
		// a checkout that converts line endings must not reach bash
		a.Script = strings.ReplaceAll(string(prologue)+string(script), "\r", "")
		list = append(list, a)
	}
	return list, nil
}
