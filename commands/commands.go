package commands

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/facundoolano/blorg/site"
)

const SRC_DIR = "src"
const TARGET_DIR = "target"
const LAYOUTS_DIR = "layouts"
const FILE_RW_MODE = 0777

func Init() error {
	// get working directory
	// default to .
	// if not exist, create directory
	// copy over default files
	fmt.Println("not implemented yet")
	return nil
}

func New() error {
	// prompt for title
	// slugify
	// fail if file already exist
	// create a new .org file with the slug
	// add front matter and org options
	fmt.Println("not implemented yet")
	return nil
}

// Read the files in src/ render them and copy the result to target/
// TODO add root dir override support
func Build() error {
	site, err := site.Load(SRC_DIR, LAYOUTS_DIR)
	if err != nil {
		return err
	}

	return buildTarget(site, true, false)
}

// TODO consider moving to site
// TODO consider making minify and reload site.config values
func buildTarget(site *site.Site, minify bool, htmlReload bool) error {
	// clear previous target contents
	os.RemoveAll(TARGET_DIR)
	os.Mkdir(TARGET_DIR, FILE_RW_MODE)

	// walk the source directory, creating directories and files at the target dir
	return filepath.WalkDir(SRC_DIR, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		subpath, _ := filepath.Rel(SRC_DIR, path)
		targetPath := filepath.Join(TARGET_DIR, subpath)

		// if it's a directory, just create the same at the target
		if entry.IsDir() {
			return os.MkdirAll(targetPath, FILE_RW_MODE)
		}

		templateFound, extension, content, err := site.RenderTemplate(path)
		if err != nil {
			return err
		}

		var contentReader io.Reader
		if templateFound {
			targetPath = strings.TrimSuffix(targetPath, filepath.Ext(targetPath)) + extension
			contentReader = bytes.NewReader(content)
		} else {
			// if no template found at location, treat the file as static
			// write its contents to target
			srcFile, err := os.Open(path)
			if err != nil {
				return err
			}
			defer srcFile.Close()
			contentReader = srcFile
		}

		// if live reload is enabled, inject the reload snippet to html files
		if htmlReload && extension == ".html" {
			// TODO inject live reload snippet
		}

		// if enabled, minify web files
		if minify && (extension == ".html" || extension == ".css" || extension == ".js") {
			// TODO minify output
		}

		// write the file contents over to target
		fmt.Println("writing", targetPath)
		return writeToFile(targetPath, contentReader)
	})
}

func writeToFile(targetPath string, source io.Reader) error {
	targetFile, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer targetFile.Close()

	_, err = io.Copy(targetFile, source)
	if err != nil {
		return err
	}

	return targetFile.Sync()
}
