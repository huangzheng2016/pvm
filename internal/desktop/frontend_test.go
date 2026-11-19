package desktop

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestFrontendAPI keeps the page's view of the backend, the Backend
// interface of api.ts, equal to the exported methods of App: a method
// the page calls must exist with as many arguments, and a method the
// page does not know is one nobody meant to expose.
func TestFrontendAPI(t *testing.T) {
	src := filepath.Join("..", "..", "cmd", "pvm-desktop", "frontend", "src")
	data, err := os.ReadFile(filepath.Join(src, "api.ts"))
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(data), "export interface Backend {")
	if !ok {
		t.Fatal("api.ts has no Backend interface")
	}
	block, _, _ = strings.Cut(block, "\n}")
	declared := map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^\s+(\w+)\((.*)\): Promise<`).FindAllStringSubmatch(block, -1) {
		args := 0
		if m[2] != "" {
			args = strings.Count(m[2], ",") + 1
		}
		declared[m[1]] = args
	}

	typ := reflect.TypeFor[*App]()
	for i := range typ.NumMethod() {
		m := typ.Method(i)
		args, ok := declared[m.Name]
		if !ok {
			t.Errorf("App.%s is missing from api.ts", m.Name)
			continue
		}
		// the receiver is the first argument of a method value
		if want := m.Type.NumIn() - 1; args != want {
			t.Errorf("api.ts calls %s with %d arguments, App.%s takes %d", m.Name, args, m.Name, want)
		}
		delete(declared, m.Name)
	}
	for name := range declared {
		t.Errorf("api.ts calls %s, which is not a method of App", name)
	}

	// api.ts is the only file that reaches the bindings
	files, err := filepath.Glob(filepath.Join(src, "*.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if filepath.Base(file) == "api.ts" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "window.go") || strings.Contains(string(data), "window.runtime") {
			t.Errorf("%s reaches the bindings directly; go through api.ts", filepath.Base(file))
		}
	}
}

// tsType is how a Go field arrives in the page.
func tsType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int64:
		return "number"
	case reflect.Slice:
		return tsType(t.Elem()) + "[]"
	}
	return t.Name()
}

// TestFrontendTypes keeps the interfaces of api.ts equal to the json
// form of the Go types they stand for: same fields, in the same order,
// with the same kind of value.
func TestFrontendTypes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "cmd", "pvm-desktop", "frontend", "src", "api.ts"))
	if err != nil {
		t.Fatal(err)
	}
	field := regexp.MustCompile(`(?m)^  (\w+): ([^;]+);`)
	for _, typ := range []reflect.Type{
		reflect.TypeFor[State](), reflect.TypeFor[VM](), reflect.TypeFor[CatalogApp](), reflect.TypeFor[Exit](),
		reflect.TypeFor[Workspace](), reflect.TypeFor[Settings](), reflect.TypeFor[Image](), reflect.TypeFor[CreateDefaults](),
		reflect.TypeFor[AppConfig](), reflect.TypeFor[CreateRequest](), reflect.TypeFor[AddExitRequest](),
		reflect.TypeFor[CreateWorkspaceRequest](), reflect.TypeFor[Step](), reflect.TypeFor[Task](), reflect.TypeFor[TaskOutput](),
	} {
		_, block, ok := strings.Cut(string(data), "export interface "+typ.Name()+" {")
		if !ok {
			t.Errorf("api.ts has no interface %s", typ.Name())
			continue
		}
		block, _, _ = strings.Cut(block, "\n}")
		var got, want []string
		for _, m := range field.FindAllStringSubmatch(block, -1) {
			kind := m[2]
			if strings.HasPrefix(kind, `"`) { // a union of the values a string takes
				kind = "string"
			}
			got = append(got, m[1]+": "+kind)
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			want = append(want, f.Tag.Get("json")+": "+tsType(f.Type))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s differs:\n api.ts %v\n Go     %v", typ.Name(), got, want)
		}
	}
}
