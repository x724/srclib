package src

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kr/fs"
	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sourcegraph/srclib/buildstore"
	"sourcegraph.com/sourcegraph/srclib/config"
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/grapher"
	"sourcegraph.com/sourcegraph/srclib/plan"
	"sourcegraph.com/sourcegraph/srclib/unit"
)

func init() {
	c, err := CLI.AddCommand("api",
		"API",
		"",
		&apiCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("describe",
		"display documentation for the def under the cursor",
		"Returns information about the definition referred to by the cursor's current position in a file.",
		&apiDescribeCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("list",
		"list all refs in a given file",
		"Return a list of all references that are in the current file.",
		&apiListCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type APICmd struct{}

var apiCmd APICmd

func (c *APICmd) Execute(args []string) error { return nil }

type APIDescribeCmd struct {
	File      string `long:"file" required:"yes" value-name:"FILE"`
	StartByte int    `long:"start-byte" required:"yes" value-name:"BYTE"`

	NoExamples bool `long:"no-examples" describe:"don't show examples from Sourcegraph.com"`
}

type APIListCmd struct {
	File string `long:"file" required:"yes" value-name:"FILE"`
}

var apiDescribeCmd APIDescribeCmd
var apiListCmd APIListCmd

// Invokes the build process on the given repository
func ensureBuild(buildStore *buildstore.RepositoryStore, repo *Repo) error {
	configOpt := config.Options{
		Repo:   string(repo.URI()),
		Subdir: ".",
	}
	toolchainExecOpt := ToolchainExecOpt{ExeMethods: "program"}

	// Config repository if not yet built.
	if _, err := buildStore.Stat(buildStore.CommitPath(repo.CommitID)); os.IsNotExist(err) {
		configCmd := &ConfigCmd{
			Options:          configOpt,
			ToolchainExecOpt: toolchainExecOpt,
			w:                os.Stderr,
		}
		if err := configCmd.Execute(nil); err != nil {
			return err
		}
	}

	// Always re-make.
	//
	// TODO(sqs): optimize this
	makeCmd := &MakeCmd{
		Options:          configOpt,
		ToolchainExecOpt: toolchainExecOpt,
	}
	if err := makeCmd.Execute(nil); err != nil {
		return err
	}

	return nil
}

// Get a list of all source units that contain the given file
func getSourceUnitsWithFile(buildStore *buildstore.RepositoryStore, repo *Repo, filename string) ([]*unit.SourceUnit, error) {
	filename = filepath.Clean(filename)

	// TODO(sqs): This whole lookup is totally inefficient. The storage format
	// is not optimized for lookups.

	// Find all source unit definition files.
	var unitFiles []string
	unitSuffix := buildstore.DataTypeSuffix(unit.SourceUnit{})
	w := fs.WalkFS(buildStore.CommitPath(repo.CommitID), buildStore)
	for w.Step() {
		if strings.HasSuffix(w.Path(), unitSuffix) {
			unitFiles = append(unitFiles, w.Path())
		}
	}

	// Find which source units the file belongs to.
	var units []*unit.SourceUnit
	for _, unitFile := range unitFiles {
		var u *unit.SourceUnit
		f, err := buildStore.Open(unitFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&u); err != nil {
			return nil, fmt.Errorf("%s: %s", unitFile, err)
		}
		for _, f2 := range u.Files {
			if filepath.Clean(f2) == filename {
				units = append(units, u)
				break
			}
		}
	}

	return units, nil
}

func (c *APIListCmd) Execute(args []string) error {
	c.File = filepath.Clean(c.File)

	repo, err := OpenRepo(filepath.Dir(c.File))
	if err != nil {
		return err
	}

	c.File, err = filepath.Rel(repo.RootDir, c.File)
	if err != nil {
		return err
	}

	if err := os.Chdir(repo.RootDir); err != nil {
		return err
	}

	buildStore, err := buildstore.NewRepositoryStore(repo.RootDir)
	if err != nil {
		return err
	}

	if err := ensureBuild(buildStore, repo); err != nil {
		return err
	}

	units, err := getSourceUnitsWithFile(buildStore, repo, c.File)
	if err != nil {
		return err
	}

	if GlobalOpt.Verbose {
		if len(units) > 0 {
			ids := make([]string, len(units))
			for i, u := range units {
				ids[i] = string(u.ID())
			}
			log.Printf("File %s is in %d source units %v.", c.File, len(units), ids)
		} else {
			log.Printf("File %s is not in any source units.", c.File)
		}
	}

	// Find the ref(s) at the character position.
	var refs []*graph.Ref
	for _, u := range units {
		var g grapher.Output
		graphFile := buildStore.FilePath(repo.CommitID, plan.SourceUnitDataFilename("graph", u))
		f, err := buildStore.Open(graphFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&g); err != nil {
			return fmt.Errorf("%s: %s", graphFile, err)
		}
		for _, ref := range g.Refs {
			if c.File == ref.File {
				refs = append(refs, ref)
			}
		}
	}

	if err := json.NewEncoder(os.Stdout).Encode(refs); err != nil {
		return err
	}
	return nil
}

func (c *APIDescribeCmd) Execute(args []string) error {
	c.File = filepath.Clean(c.File)

	repo, err := OpenRepo(filepath.Dir(c.File))
	if err != nil {
		return err
	}

	c.File, err = filepath.Rel(repo.RootDir, c.File)
	if err != nil {
		return err
	}

	if err := os.Chdir(repo.RootDir); err != nil {
		return err
	}

	buildStore, err := buildstore.NewRepositoryStore(repo.RootDir)
	if err != nil {
		return err
	}

	if err := ensureBuild(buildStore, repo); err != nil {
		return err
	}

	units, err := getSourceUnitsWithFile(buildStore, repo, c.File)
	if err != nil {
		return err
	}

	if GlobalOpt.Verbose {
		if len(units) > 0 {
			ids := make([]string, len(units))
			for i, u := range units {
				ids[i] = string(u.ID())
			}
			log.Printf("Position %s:%d is in %d source units %v.", c.File, c.StartByte, len(units), ids)
		} else {
			log.Printf("Position %s:%d is not in any source units.", c.File, c.StartByte)
		}
	}

	// Find the ref(s) at the character position.
	var ref *graph.Ref
OuterLoop:
	for _, u := range units {
		var g grapher.Output
		graphFile := buildStore.FilePath(repo.CommitID, plan.SourceUnitDataFilename("graph", u))
		f, err := buildStore.Open(graphFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&g); err != nil {
			return fmt.Errorf("%s: %s", graphFile, err)
		}
		for _, ref2 := range g.Refs {
			if c.File == ref2.File && c.StartByte >= ref2.Start && c.StartByte <= ref2.End {
				ref = ref2
				if ref.DefUnit == "" {
					ref.DefUnit = u.Name
				}
				if ref.DefUnitType == "" {
					ref.DefUnitType = u.Type
				}
				break OuterLoop
			}
		}
	}

	if ref == nil {
		if GlobalOpt.Verbose {
			log.Printf("No ref found at %s:%d.", c.File, c.StartByte)
		}
		fmt.Println(`{}`)
		return nil
	}

	if ref.DefRepo == "" {
		ref.DefRepo = repo.URI()
	}

	var resp struct {
		Def      *sourcegraph.Def
		Examples []*sourcegraph.Example
	}

	// Now find the def for this ref.
	defInCurrentRepo := ref.DefRepo == repo.URI()
	if defInCurrentRepo {
		// Def is in the current repo.
		var g grapher.Output
		graphFile := buildStore.FilePath(repo.CommitID, plan.SourceUnitDataFilename("graph", &unit.SourceUnit{Name: ref.DefUnit, Type: ref.DefUnitType}))
		f, err := buildStore.Open(graphFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&g); err != nil {
			return fmt.Errorf("%s: %s", graphFile, err)
		}
		for _, def2 := range g.Defs {
			if def2.Path == ref.DefPath {
				resp.Def = &sourcegraph.Def{Def: *def2}
				break
			}
		}
		if resp.Def != nil {
			for _, doc := range g.Docs {
				if doc.Path == ref.DefPath {
					resp.Def.DocHTML = doc.Data
				}
			}

			// If Def is in the current Repo, transform that path to be an absolute path
			resp.Def.File = filepath.Join(repo.RootDir, resp.Def.File)
		}
		if resp.Def == nil && GlobalOpt.Verbose {
			log.Printf("No definition found with path %q in unit %q type %q.", ref.DefPath, ref.DefUnit, ref.DefUnitType)
		}
	}

	spec := sourcegraph.DefSpec{
		Repo:     string(ref.DefRepo),
		UnitType: ref.DefUnitType,
		Unit:     ref.DefUnit,
		Path:     string(ref.DefPath),
	}

	var wg sync.WaitGroup

	if resp.Def == nil {
		// Def is not in the current repo. Try looking it up using the
		// Sourcegraph API.
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			resp.Def, _, err = apiclient.Defs.Get(spec, &sourcegraph.DefGetOptions{Doc: true})
			if err != nil && GlobalOpt.Verbose {
				log.Printf("Couldn't fetch definition %v: %s.", spec, err)
			}
		}()
	}

	if !c.NoExamples {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			resp.Examples, _, err = apiclient.Defs.ListExamples(spec, &sourcegraph.DefListExamplesOptions{
				Formatted:   true,
				ListOptions: sourcegraph.ListOptions{PerPage: 4},
			})
			if err != nil && GlobalOpt.Verbose {
				log.Printf("Couldn't fetch examples for %v: %s.", spec, err)
			}
		}()
	}

	wg.Wait()

	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return err
	}
	return nil
}
