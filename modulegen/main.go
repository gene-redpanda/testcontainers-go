package main

import (
	"flag"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/testcontainers/testcontainers-go/modulegen/internal/dependabot"
	"github.com/testcontainers/testcontainers-go/modulegen/internal/mkdocs"
	"github.com/testcontainers/testcontainers-go/modulegen/internal/tools"
	"github.com/testcontainers/testcontainers-go/modulegen/internal/workflow"
)

var (
	asModuleVar  bool
	nameVar      string
	nameTitleVar string
	imageVar     string
)

var templates = []string{
	"docs_example.md", "example_test.go", "example.go", "go.mod", "Makefile",
}

func init() {
	flag.StringVar(&nameVar, "name", "", "Name of the example. Only alphabetical characters are allowed.")
	flag.StringVar(&nameTitleVar, "title", "", "(Optional) Title of the example name, used to override the name in the case of mixed casing (Mongodb -> MongoDB). Use camel-case when needed. Only alphabetical characters are allowed.")
	flag.StringVar(&imageVar, "image", "", "Fully-qualified name of the Docker image to be used by the example")
	flag.BoolVar(&asModuleVar, "as-module", false, "If set, the example will be generated as a Go module, under the modules directory. Otherwise, it will be generated as a subdirectory of the examples directory.")
}

type Example struct {
	Image     string // fully qualified name of the Docker image
	IsModule  bool   // if true, the example will be generated as a Go module
	Name      string
	TitleName string // title of the name: e.g. "mongodb" -> "MongoDB"
	TCVersion string // Testcontainers for Go version
}

// ContainerName returns the name of the container, which is the lower-cased title of the example
// If the title is set, it will be used instead of the name
func (e *Example) ContainerName() string {
	name := e.Lower()

	if e.IsModule {
		name = e.Title()
	} else {
		if e.TitleName != "" {
			r, n := utf8.DecodeRuneInString(e.TitleName)
			name = string(unicode.ToLower(r)) + e.TitleName[n:]
		}
	}

	return name + "Container"
}

// Entrypoint returns the name of the entrypoint function, which is the lower-cased title of the example
// If the example is a module, the entrypoint will be "RunContainer"
func (e *Example) Entrypoint() string {
	if e.IsModule {
		return "RunContainer"
	}

	return "runContainer"
}

func (e *Example) Lower() string {
	return strings.ToLower(e.Name)
}

func (e *Example) ParentDir() string {
	if e.IsModule {
		return "modules"
	}

	return "examples"
}

func (e *Example) Title() string {
	if e.TitleName != "" {
		return e.TitleName
	}

	return cases.Title(language.Und, cases.NoLower).String(e.Lower())
}

func (e *Example) Type() string {
	if e.IsModule {
		return "module"
	}
	return "example"
}

func (e *Example) Validate() error {
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`).MatchString(e.Name) {
		return fmt.Errorf("invalid name: %s. Only alphanumerical characters are allowed (leading character must be a letter)", e.Name)
	}

	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`).MatchString(e.TitleName) {
		return fmt.Errorf("invalid title: %s. Only alphanumerical characters are allowed (leading character must be a letter)", e.TitleName)
	}

	return nil
}

func main() {
	required := []string{"name", "image"}
	flag.Parse()

	seen := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	for _, req := range required {
		if !seen[req] {
			// or possibly use `log.Fatalf` instead of:
			fmt.Fprintf(os.Stderr, "missing required -%s argument/flag\n", req)
			os.Exit(2) // the same exit code flag.Parse uses
		}
	}

	currentDir, err := filepath.Abs(filepath.Dir("."))
	if err != nil {
		fmt.Printf(">> could not get the root dir: %v\n", err)
		os.Exit(1)
	}

	ctx := NewContext(filepath.Dir(currentDir))

	mkdocsConfig, err := mkdocs.ReadConfig(ctx.MkdocsConfigFile())
	if err != nil {
		fmt.Printf(">> could not read MkDocs config: %v\n", err)
		os.Exit(1)
	}

	example := Example{
		Image:     imageVar,
		IsModule:  asModuleVar,
		Name:      nameVar,
		TitleName: nameTitleVar,
		TCVersion: mkdocsConfig.Extra.LatestVersion,
	}

	err = generate(example, ctx)
	if err != nil {
		fmt.Printf(">> error generating the example: %v\n", err)
		os.Exit(1)
	}

	cmdDir := filepath.Join(ctx.RootDir, example.ParentDir(), example.Lower())
	err = tools.GoModTidy(cmdDir)
	if err != nil {
		fmt.Printf(">> error synchronizing the dependencies: %v\n", err)
		os.Exit(1)
	}
	err = tools.GoVet(cmdDir)
	if err != nil {
		fmt.Printf(">> error checking generated code: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Please go to", cmdDir, "directory to check the results, where 'go mod tidy' and 'go vet' was executed to synchronize the dependencies")
	fmt.Println("Commit the modified files and submit a pull request to include them into the project")
	fmt.Println("Thanks!")
}

func generate(example Example, ctx *Context) error {
	if err := example.Validate(); err != nil {
		return err
	}

	outputDir := filepath.Join(ctx.RootDir, example.ParentDir())
	docsOuputDir := filepath.Join(ctx.DocsDir(), example.ParentDir())

	funcMap := template.FuncMap{
		"Entrypoint":    func() string { return example.Entrypoint() },
		"ContainerName": func() string { return example.ContainerName() },
		"ExampleType":   func() string { return example.Type() },
		"ParentDir":     func() string { return example.ParentDir() },
		"ToLower":       func() string { return example.Lower() },
		"Title":         func() string { return example.Title() },
		"codeinclude":   func(s string) template.HTML { return template.HTML(s) }, // escape HTML comments for codeinclude
	}

	exampleLower := example.Lower()

	// create the example dir
	err := os.MkdirAll(filepath.Join(outputDir, exampleLower), 0o700)
	if err != nil {
		return err
	}

	for _, tmpl := range templates {
		name := tmpl + ".tmpl"
		t, err := template.New(name).Funcs(funcMap).ParseFiles(filepath.Join("_template", name))
		if err != nil {
			return err
		}

		// initialize the data using the example struct, which is the default data to be used while
		// doing the interpolation of the data and the template
		var data any

		syncDataFn := func() any {
			return example
		}

		// create a new file
		var exampleFilePath string

		if strings.EqualFold(tmpl, "docs_example.md") {
			// docs example file will go into the docs directory
			exampleFilePath = filepath.Join(docsOuputDir, exampleLower+".md")
		} else {
			exampleFilePath = filepath.Join(outputDir, exampleLower, strings.ReplaceAll(tmpl, "example", exampleLower))
		}

		err = os.MkdirAll(filepath.Dir(exampleFilePath), 0o777)
		if err != nil {
			return err
		}

		exampleFile, _ := os.Create(exampleFilePath)
		defer exampleFile.Close()

		data = syncDataFn()

		err = t.ExecuteTemplate(exampleFile, name, data)
		if err != nil {
			return err
		}
	}
	// update github ci workflow
	err = generateWorkFlow(ctx)
	if err != nil {
		return err
	}
	// update examples in mkdocs
	err = generateMkdocs(ctx, example)
	if err != nil {
		return err
	}
	// update examples in dependabot
	err = generateDependabotUpdates(ctx, example)
	if err != nil {
		return err
	}
	return nil
}

func generateDependabotUpdates(ctx *Context, example Example) error {
	// update examples in dependabot
	directory := "/" + example.ParentDir() + "/" + example.Lower()
	return dependabot.UpdateConfig(ctx.DependabotConfigFile(), directory, "gomod")
}

func generateMkdocs(ctx *Context, example Example) error {
	// update examples in mkdocs
	exampleMd := example.ParentDir() + "/" + example.Lower() + ".md"
	indexMd := example.ParentDir() + "/index.md"
	return mkdocs.UpdateConfig(ctx.MkdocsConfigFile(), example.IsModule, exampleMd, indexMd)
}

func generateWorkFlow(ctx *Context) error {
	rootCtx, err := getRootContext()
	if err != nil {
		return err
	}
	examples, err := rootCtx.GetExamples()
	if err != nil {
		return err
	}
	modules, err := rootCtx.GetModules()
	if err != nil {
		return err
	}
	return workflow.Generate(ctx.GithubWorkflowsDir(), examples, modules)
}

func getRootContext() (*Context, error) {
	current, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return NewContext(filepath.Dir(current)), nil
}
