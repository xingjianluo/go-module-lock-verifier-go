package lockfile

import (
	"bytes"
	"errors"
	"testing"
)

func sampleLock() *Lock {
	return &Lock{
		FormatVersion: FormatVersion,
		MainModule:    "example.com/main",
		ManifestHash:  "sha256:" + stringsRepeat("a", 64),
		Entries: []Entry{
			{
				Path: "b.com/m", Version: "v1.2.0", Direct: true,
				GoModDigest:  "sha256:" + stringsRepeat("1", 64),
				TreeDigest:   "sha256:" + stringsRepeat("2", 64),
				IntroducedBy: []string{"example.com/main"},
			},
			{
				Path: "a.com/m", Version: "v0.9.0", Direct: false,
				Replaced:     true,
				ReplaceSource: "a.com/m@",
				GoModDigest:  "sha256:" + stringsRepeat("3", 64),
				TreeDigest:   "sha256:" + stringsRepeat("4", 64),
				IntroducedBy: []string{"example.com/main", "b.com/m@v1.2.0"},
			},
		},
	}
}

func stringsRepeat(c string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c[0]
	}
	return string(b)
}

func TestMarshalDeterministicAndTamperDetected(t *testing.T) {
	l1 := sampleLock()
	d1, err := Marshal(l1)
	if err != nil {
		t.Fatal(err)
	}
	// Marshal again from an equivalent, differently-ordered lock.
	l2 := sampleLock()
	l2.Entries[0], l2.Entries[1] = l2.Entries[1], l2.Entries[0]
	d2, err := Marshal(l2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d1, d2) {
		t.Fatalf("locking is not byte-deterministic\n%s\n----\n%s", d1, d2)
	}
	t.Logf("lock bytes:\n%s", d1)

	parsed, err := Unmarshal(d1)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.VerifyIntegrity(); err != nil {
		t.Fatalf("fresh lock must verify: %v", err)
	}
	if parsed.Entries[0].Path != "a.com/m" {
		t.Fatalf("entries not sorted, got %q", parsed.Entries[0].Path)
	}

	// Tamper with a version but leave the hash: must be LockErrTampered.
	tampered := bytes.ReplaceAll(d1, []byte("v1.2.0"), []byte("v1.2.1"))
	tl, err := Unmarshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	err = tl.VerifyIntegrity()
	if err == nil {
		t.Fatal("tampered lock passed integrity check")
	}
	le, ok := AsError(err)
	if !ok || le.Code != LockErrTampered {
		t.Fatalf("want tampered, got %v (%v)", le, err)
	}
	if !errors.Is(err, &Error{Code: LockErrTampered}) {
		t.Fatal("errors.Is classification failed")
	}
	t.Logf("tamper classified: %v", err)
}

func TestStaleVsTamperedDistinguishable(t *testing.T) {
	l := sampleLock()
	d, _ := Marshal(l)
	parsed, _ := Unmarshal(d)
	// Changing only ManifestHash and recomputing self hash models "stale":
	// body is internally consistent, but declaration hash differs. The lock
	// itself cannot classify stale-ness; verify package compares manifests.
	parsed.ManifestHash = "sha256:" + stringsRepeat("f", 64)
	parsed.LockHash = parsed.SelfHash()
	if err := parsed.VerifyIntegrity(); err != nil {
		t.Fatalf("re-hashed consistent body must verify: %v", err)
	}
	t.Log("consistent body with changed manifest hash passes integrity (stale is external comparison)")
}

func TestMalformedAndDuplicate(t *testing.T) {
	bad := [][]byte{
		[]byte(``),
		[]byte(`{not json`),
		[]byte(`{"format_version":"gmlv9","main_module":"x","manifest_hash":"sha256:` + stringsRepeat("a", 64) +
			`","entries":[],"lock_hash":"sha256:` + stringsRepeat("b", 64) + `"}`),
	}
	for _, b := range bad {
		if _, err := Unmarshal(b); err == nil {
			t.Fatalf("expected malformed for %q", b)
		} else {
			t.Logf("rejected: %v", err)
		}
	}

	l := sampleLock()
	l.Entries = append(l.Entries, l.Entries[0])
	if _, err := Marshal(l); err == nil {
		t.Fatal("expected duplicate entry error")
	} else {
		le, _ := AsError(err)
		if le.Code != LockErrDuplicateEntry {
			t.Fatalf("want duplicate, got %v", err)
		}
		t.Logf("duplicate classified: %v", err)
	}
}
