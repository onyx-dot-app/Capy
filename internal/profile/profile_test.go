package profile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadNormalizesWhatAFileHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	aliases := `"Annie","Annie"," Ann ","","‮X"`
	for i := range 12 {
		aliases += `,"a` + strings.Repeat("x", i) + `"`
	}
	body := `{"name":"  Ana\u0007 ` + strings.Repeat("é", 150) + `","aliases":[` + aliases + `]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Read(path)
	if !strings.HasPrefix(p.Name, "Ana ") || utf8.RuneCountInString(p.Name) != MaxNameRunes-2 {
		t.Fatalf("name %q (%d runes): want the control dropped, cut to %d runes, then trimmed", p.Name, utf8.RuneCountInString(p.Name), MaxNameRunes)
	}
	if len(p.Aliases) != MaxAliases || !slices.Equal(p.Aliases[:3], []string{"Annie", "Ann", "X"}) {
		t.Fatalf("aliases %q: want no repeats, blanks or format characters, at most %d", p.Aliases, MaxAliases)
	}
}

func TestSaveRoundTripsPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	saved, err := Save(path, Profile{Name: " Alex Rivera ", Aliases: []string{"Alex", "Alex", "Capy team"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Profile{Name: "Alex Rivera", Aliases: []string{"Alex", "Capy team"}}
	if !equal(saved, want) || !equal(Read(path), want) {
		t.Fatalf("saved %+v, read %+v; want %+v", saved, Read(path), want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", fi.Mode(), err)
	}
	if got := Read(path).Names(); !slices.Equal(got, []string{"Alex Rivera", "Alex", "Capy team"}) {
		t.Fatalf("Names = %q", got)
	}
	// Cleared, the name stays cleared: the account's is only a start.
	accountName = func() string { return "Account Name" }
	t.Cleanup(func() { accountName = func() string { return "" } })
	if _, err := Save(path, Profile{}); err != nil {
		t.Fatal(err)
	}
	if p := Read(path); p.Name != "" || len(p.Names()) != 0 {
		t.Fatalf("cleared profile read as %+v", p)
	}
}

func TestAProfileNotSavedStartsWithTheAccountsName(t *testing.T) {
	accountName = func() string { return " Alex Rivera\n" }
	t.Cleanup(func() { accountName = func() string { return "" } })
	dir := t.TempDir()
	if p := Read(filepath.Join(dir, FileName)); p.Name != "Alex Rivera" || len(p.Aliases) != 0 {
		t.Fatalf("missing file read as %+v", p)
	}
	// A file Capy cannot use is no profile at all, not the account's.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := Read(bad); p.Name != "" {
		t.Fatalf("malformed file read as %+v", p)
	}
}

func TestSaveFailsWithoutNamingThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-folder-of-Ana", FileName)
	_, err := Save(path, Profile{Name: "Ana"})
	if err == nil || strings.Contains(err.Error(), "Ana") {
		t.Fatalf("save to a missing folder: %v", err)
	}
}

func equal(a, b Profile) bool { return a.Name == b.Name && slices.Equal(a.Aliases, b.Aliases) }
