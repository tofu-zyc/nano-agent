package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledLayoutSuffixAndFileIdentity(t *testing.T) {
	for _, osName := range []string{"linux", "windows"} {
		t.Run(osName, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "bin")
			if err := os.MkdirAll(bin, 0700); err != nil {
				t.Fatal(err)
			}
			name := "nano-gateway"
			if osName == "windows" {
				name += ".exe"
			}
			self := filepath.Join(bin, name)
			if err := os.WriteFile(self, []byte("binary"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := installedExeAt(self, home, osName); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(home, "alias")
			if err := os.Link(self, alias); err == nil {
				if _, err := installedExeAt(alias, home, osName); err != nil {
					t.Fatal("same file alias rejected:", err)
				}
			}
			other := filepath.Join(home, "source")
			if err := os.WriteFile(other, []byte("binary"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := installedExeAt(other, home, osName); err == nil {
				t.Fatal("source copy recognized as installed")
			}
		})
	}
}
