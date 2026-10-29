package main

import (
	"path/filepath"
	"testing"
)

func TestParseVMFlags(t *testing.T) {
	cases := []struct {
		args      []string
		name, mem string
		cpus      int
		wantErr   bool
	}{
		{nil, "", "2G", 2, false},
		{[]string{"test"}, "test", "2G", 2, false},
		{[]string{"test", "--cpu", "4", "--mem", "8G"}, "test", "8G", 4, false},
		{[]string{"--mem", "512M", "test"}, "test", "512M", 2, false},
		{[]string{"a", "b"}, "", "", 0, true},
		{[]string{"--cpu", "0"}, "", "", 0, true},
		{[]string{"--mem", "lots"}, "", "", 0, true},
	}
	for _, c := range cases {
		name, cpus, mem, err := parseVMFlags(c.args)
		if c.wantErr {
			if err == nil {
				t.Errorf("%v: want error", c.args)
			}
			continue
		}
		if err != nil || name != c.name || cpus != c.cpus || mem != c.mem {
			t.Errorf("%v: got %q %d %q %v", c.args, name, cpus, mem, err)
		}
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"test", "quick-fox", "vm2"} {
		if err := checkName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a", "box", "Test", "-x", "x-", "a.b", "1abc", "a--b"} {
		if checkName(bad) == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestAddresses(t *testing.T) {
	v := VM{Index: 2}
	if v.IP() != "10.42.0.2" || v.MAC() != "02:42:0a:2a:00:02" {
		t.Errorf("index 2: %s %s", v.IP(), v.MAC())
	}
	v = VM{Index: 300}
	if v.IP() != "10.42.1.44" || v.MAC() != "02:42:0a:2a:01:2c" {
		t.Errorf("index 300: %s %s", v.IP(), v.MAC())
	}
}

func TestInsertUsesLowestFreeIndex(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "box.db"))
	if err != nil {
		t.Fatal(err)
	}
	insert := func(name string) int {
		v := VM{Name: name, Owner: "u", CPUs: 1, Mem: "1G"}
		if err := s.Insert(&v); err != nil {
			t.Fatal(err)
		}
		return v.Index
	}
	if i := insert("a"); i != 2 {
		t.Fatalf("first: %d", i)
	}
	insert("b")
	insert("c")
	s.Delete("b")
	if i := insert("d"); i != 3 {
		t.Errorf("reuse gap: got %d, want 3", i)
	}
	if i := insert("e"); i != 5 {
		t.Errorf("after gap: got %d, want 5", i)
	}
	s.Delete("a")
	if i := insert("f"); i != 2 {
		t.Errorf("reuse 2: got %d", i)
	}
}

func TestTapNameFitsIfname(t *testing.T) {
	// Linux interface names are at most 15 characters.
	for _, v := range []VM{{Name: "bright-squirrel", Index: 2}, {Name: "x", Index: 65534}} {
		if n := tap(v); len(n) > 15 {
			t.Errorf("%s: tap name %q too long", v.Name, n)
		}
	}
}

// A consistent copy of the database, taken while boxd runs (for backups).
func TestDumpDatabase(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(filepath.Join(dir, "box.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Insert(&VM{Name: "keep-me", Owner: "u", CPUs: 1, Mem: "1G"})
	if err := s.Dump(filepath.Join(dir, "copy.db")); err != nil {
		t.Fatal(err)
	}
	c, err := openStore(filepath.Join(dir, "copy.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("keep-me"); err != nil {
		t.Errorf("the copy lacks the VM: %v", err)
	}
}

// Every VM records the image it was born from (a copy: its source's).
func TestVMRecordsItsImage(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "box.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Insert(&VM{Name: "a", Owner: "u", CPUs: 1, Mem: "1G", Image: "exe/images/boxuntu-1@base"})
	v, err := s.Get("a")
	if err != nil || v.Image != "exe/images/boxuntu-1@base" {
		t.Errorf("image %q, %v", v.Image, err)
	}
}
