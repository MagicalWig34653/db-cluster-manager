// SPDX-License-Identifier: AGPL-3.0-only

// Command composelab is the lab harness of E17 part b (image composer, ADR
// 0026). It runs on the host and only reads the embedded catalog or writes
// files; the builds and package queries happen in lab VMs.
//
//	composelab audit -out DIR      one package list per distribution release:
//	                               every package spec the catalog can emit
//	                               (component versions and features), as the
//	                               generator renders it for that family.
//	composelab gen -recipe FILE -out DIR
//	                               render a recipe's Dockerfile and context.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagecompose"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: composelab audit|gen ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		out := fs.String("out", "", "output directory")
		_ = fs.Parse(os.Args[2:])
		must(audit(*out))
	case "gen":
		fs := flag.NewFlagSet("gen", flag.ExitOnError)
		recipe := fs.String("recipe", "", "recipe JSON")
		out := fs.String("out", "", "output directory")
		_ = fs.Parse(os.Args[2:])
		must(gen(*recipe, *out))
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "composelab:", err)
		os.Exit(1)
	}
}

func spec(family, name, constraint string) string {
	if constraint == "" {
		return name
	}
	switch family {
	case "alpine":
		return name + "~=" + constraint
	case "debian":
		return name + "=" + constraint + ".*"
	}
	return name
}

func audit(out string) error {
	if out == "" {
		return fmt.Errorf("-out required")
	}
	c := imagecompose.Default()
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, d := range c.Distros {
		for _, r := range d.Releases {
			seen := map[string]bool{}
			var lines []string
			emit := func(comp, ver, feat string, modules []string, s string) {
				l := strings.Join([]string{d.Family, comp, ver, feat, strings.Join(modules, ","), s}, "|")
				if !seen[l] {
					seen[l] = true
					lines = append(lines, l)
				}
			}
			for _, comp := range c.Components {
				for _, cv := range comp.Versions {
					su, ok := cv.SupportFor(d.ID, r.ID)
					if !ok {
						continue
					}
					for _, p := range su.Packages {
						emit(comp.ID, cv.Version, "", su.Modules, spec(d.Family, p.Name, p.Constraint))
					}
					for _, f := range comp.Features {
						if su.Lacks(f.ID) {
							continue
						}
						for _, tpl := range f.Packages[d.Family] {
							emit(comp.ID, cv.Version, f.ID, su.Modules, strings.ReplaceAll(tpl, "{php}", su.Prefix))
						}
					}
				}
			}
			sort.Strings(lines)
			name := filepath.Join(out, d.ID+"-"+r.ID+".tsv")
			if err := os.WriteFile(name, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				return err
			}
			fmt.Printf("%s: %d package specs\n", name, len(lines))
		}
	}
	return nil
}

func gen(recipe, out string) error {
	if recipe == "" || out == "" {
		return fmt.Errorf("-recipe and -out required")
	}
	b, err := os.ReadFile(recipe)
	if err != nil {
		return err
	}
	var r imagecompose.Recipe
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	res, err := imagecompose.Default().Generate(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "Dockerfile"), []byte(res.Rendered.Dockerfile), 0o644); err != nil {
		return err
	}
	for _, f := range res.Rendered.Files {
		p := filepath.Join(out, filepath.Clean("/"+f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if f.Mode != 0 {
			mode = os.FileMode(f.Mode)
		}
		if err := os.WriteFile(p, []byte(f.Content), mode); err != nil {
			return err
		}
	}
	fmt.Printf("role=%s digest=%s packages=%d warnings=%d\n", res.Role, res.Digest, len(res.Packages), len(res.Warnings))
	for _, w := range res.Warnings {
		fmt.Println("warning:", w)
	}
	return nil
}
