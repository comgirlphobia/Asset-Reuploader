package files

import (
	"os"
	"path/filepath"
)

func Write(n, c string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}

	f, err := os.Create(filepath.Join(dir, n))
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteString(c); err != nil {
		return err
	}
	return err
}

func Read(n string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(filepath.Join(dir, n))
	return string(data), err
}
