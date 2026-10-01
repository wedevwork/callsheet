package workspacetransfer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"golang.org/x/sys/unix"

	"github.com/wedevwork/callsheet/internal/contract"
)

// UT-2: Git-compatible cleanliness for the supported set: golden
// expectations of git status, except that normalized content wins (the
// owner's W1 clarification): a file whose working-tree bytes clean to the
// indexed blob is clean even where git status lists it for line-ending or
// stat-only reasons.

// status runs the cleanliness check of r for goos with env.
func status(t *testing.T, r *fixtureRepo, goos string, env Env) error {
	t.Helper()
	rp, err := openRepo(context.Background(), env, r.root)
	if err != nil {
		return err
	}
	defer rp.close()
	_, err = checkClean(context.Background(), fastDeps(), rp, env, goos)
	return err
}

func wantClean(t *testing.T, r *fixtureRepo, goos string, env Env, what string) {
	t.Helper()
	if err := status(t, r, goos, env); err != nil {
		t.Fatalf("%s: want clean, got %v", what, err)
	}
}

func wantDirty(t *testing.T, r *fixtureRepo, goos string, env Env, what string) {
	t.Helper()
	err := status(t, r, goos, env)
	if contract.TransferReason(err) != contract.ReasonDirtySource || contract.CodeOf(err) != contract.CodeConflict {
		t.Fatalf("%s: want dirty, got %v", what, err)
	}
}

func wantUnsupported(t *testing.T, r *fixtureRepo, goos string, env Env, what string) {
	t.Helper()
	err := status(t, r, goos, env)
	if contract.TransferReason(err) != contract.ReasonUnsupportedRepository || contract.CodeOf(err) != contract.CodeInvalidArgument {
		t.Fatalf("%s: want unsupported, got %v", what, err)
	}
}

func baseFiles() map[string]fspec {
	return map[string]fspec{"a.txt": reg("alpha\n"), "dir/b.txt": reg("beta\n"), "run.sh": exe("#!/bin/sh\n"), "ln": link("a.txt")}
}

func TestCleanlinessStagedAndUnstaged(t *testing.T) {
	env := emptyEnv(t)
	g := "linux"
	r := newRepo(t, baseFiles())
	wantClean(t, r, g, env, "committed")
	// Unstaged content, delete and type changes.
	r.writeFile("a.txt", reg("changed\n"))
	wantDirty(t, r, g, env, "unstaged content")
	r.writeFile("a.txt", reg("alpha\n"))
	wantClean(t, r, g, env, "restored")
	os.Remove(filepath.Join(r.root, "dir/b.txt"))
	wantDirty(t, r, g, env, "unstaged delete")
	r.writeFile("dir/b.txt", reg("beta\n"))
	r.writeFile("a.txt", link("dir/b.txt"))
	wantDirty(t, r, g, env, "unstaged type change")
	r.writeFile("a.txt", reg("alpha\n"))
	os.Remove(filepath.Join(r.root, "ln"))
	os.WriteFile(filepath.Join(r.root, "ln"), []byte("a.txt"), 0o644)
	wantDirty(t, r, g, env, "symlink replaced by a file with symlinks on")
	r.writeFile("ln", link("a.txt"))
	os.Remove(filepath.Join(r.root, "dir/b.txt"))
	os.Mkdir(filepath.Join(r.root, "dir/b.txt"), 0o755)
	wantDirty(t, r, g, env, "file replaced by a directory")
	os.Remove(filepath.Join(r.root, "dir/b.txt"))
	r.writeFile("dir/b.txt", reg("beta\n"))
	os.Remove(filepath.Join(r.root, "dir/b.txt"))
	unix.Mkfifo(filepath.Join(r.root, "dir/b.txt"), 0o644)
	wantDirty(t, r, g, env, "tracked path is a FIFO (never opened)")
	os.Remove(filepath.Join(r.root, "dir/b.txt"))
	r.writeFile("dir/b.txt", reg("beta\n"))
	wantClean(t, r, g, env, "restored again")
	// Staged add, delete, mode and content.
	files := baseFiles()
	files["new.txt"] = reg("new\n")
	r.writeFile("new.txt", reg("new\n"))
	r.writeIndex(files)
	wantDirty(t, r, g, env, "staged add")
	os.Remove(filepath.Join(r.root, "new.txt"))
	delete(files, "new.txt")
	delete(files, "a.txt")
	r.writeIndex(files)
	wantDirty(t, r, g, env, "staged delete (file still on disk)")
	files = baseFiles()
	files["run.sh"] = reg("#!/bin/sh\n")
	r.writeIndex(files)
	wantDirty(t, r, g, env, "staged mode change")
	// A staged change undone in the worktree is still dirty.
	files = baseFiles()
	files["a.txt"] = reg("staged\n")
	r.store.SetEncodedObject(blobObject("staged\n"))
	r.writeIndex(files)
	wantDirty(t, r, g, env, "staged change cancelled by the worktree")
	r.writeIndex(baseFiles())
	wantClean(t, r, g, env, "index restored")
	// An intent-to-add entry is dirty.
	entries := r.indexEntries(baseFiles())
	entries = append(entries, &index.Entry{Name: "ita.txt", Mode: filemode.Regular, Hash: plumbing.ComputeHash(plumbing.BlobObject, nil), IntentToAdd: true})
	r.writeFile("ita.txt", reg(""))
	r.writeIndexV3(entries)
	wantDirty(t, r, g, env, "intent-to-add")
	os.Remove(filepath.Join(r.root, "ita.txt"))
	r.writeIndex(baseFiles())
	// Untracked files: nonignored dirty, ignored clean, empty dirs and
	// special files invisible.
	os.MkdirAll(filepath.Join(r.root, "empty/deeper"), 0o755)
	unix.Mkfifo(filepath.Join(r.root, "fifo"), 0o644)
	wantClean(t, r, g, env, "empty directories and a FIFO")
	r.writeFile("untracked/u.txt", reg("u"))
	wantDirty(t, r, g, env, "untracked file in a new directory")
	r.writeFile(".gitignore", reg("untracked/\nfifo\n"))
	r.commit(map[string]fspec{"a.txt": reg("alpha\n"), "dir/b.txt": reg("beta\n"), "run.sh": exe("#!/bin/sh\n"), "ln": link("a.txt"), ".gitignore": reg("untracked/\nfifo\n")}, "ignore")
	wantClean(t, r, g, env, "ignored untracked directory")
	r.writeFile("stray.txt", reg("x"))
	wantDirty(t, r, g, env, "untracked file")
	os.Remove(filepath.Join(r.root, "stray.txt"))
	os.Symlink("nowhere", filepath.Join(r.root, "strayLink"))
	wantDirty(t, r, g, env, "untracked symlink")
	os.Remove(filepath.Join(r.root, "strayLink"))
	wantClean(t, r, g, env, "clean again")
}

func blobObject(s string) plumbing.EncodedObject {
	o := &plumbing.MemoryObject{}
	o.SetType(plumbing.BlobObject)
	o.Write([]byte(s))
	return o
}

// writeIndexV3 writes a v3 index (extended flags).
func (r *fixtureRepo) writeIndexV3(entries []*index.Entry) {
	r.t.Helper()
	var buf bytes.Buffer
	if err := index.NewEncoder(&buf).Encode(&index.Index{Version: 3, Entries: entries}); err != nil {
		r.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.root, ".git/index"), buf.Bytes(), 0o644)
}

func TestCleanlinessIgnoredTracked(t *testing.T) {
	env := emptyEnv(t)
	files := baseFiles()
	files[".gitignore"] = reg("*.log\n")
	files["kept.log"] = reg("tracked but ignored\n")
	r := newRepo(t, files)
	wantClean(t, r, "linux", env, "committed ignored tracked file")
	r.writeFile("kept.log", reg("edited\n"))
	wantDirty(t, r, "linux", env, "edit to an ignored tracked file")
	r.writeFile("kept.log", reg("tracked but ignored\n"))
	r.writeFile("other.log", reg("x"))
	wantClean(t, r, "linux", env, "ignored untracked log")
}

func TestCleanlinessHead(t *testing.T) {
	env := emptyEnv(t)
	r := newRepo(t, baseFiles())
	mustWrite(t, filepath.Join(r.root, ".git/HEAD"), r.head.String()+"\n")
	wantClean(t, r, "linux", env, "detached HEAD")
	u := newRepo(t, nil)
	err := status(t, u, "linux", env)
	if contract.TransferReason(err) != contract.ReasonDirtySource || !bytes.Contains([]byte(err.Error()), []byte("commit first")) {
		t.Fatalf("unborn: %v", err)
	}
}

func TestCleanlinessModesAndSymlinks(t *testing.T) {
	env := emptyEnv(t)
	r := newRepo(t, baseFiles())
	os.Chmod(filepath.Join(r.root, "a.txt"), 0o755)
	wantDirty(t, r, "linux", env, "executable bit with core.fileMode default")
	r.config("[core]\n\tfilemode = false\n")
	wantClean(t, r, "linux", env, "executable bit with core.fileMode false")
	os.Chmod(filepath.Join(r.root, "a.txt"), 0o644)
	os.Chmod(filepath.Join(r.root, "run.sh"), 0o644)
	wantClean(t, r, "linux", env, "lost executable bit with core.fileMode false")
	r2 := newRepo(t, baseFiles())
	os.Chmod(filepath.Join(r2.root, "a.txt"), 0o654)
	wantClean(t, r2, "linux", env, "group/other execute bits are not the mode")
	os.Chmod(filepath.Join(r2.root, "a.txt"), 0o600)
	wantClean(t, r2, "linux", env, "permission bits are irrelevant")
	// Broken symlinks are valid; the link text is compared, never
	// followed.
	r3 := newRepo(t, map[string]fspec{"broken": link("../../outside/nowhere"), "abs": link("/etc/passwd")})
	wantClean(t, r3, "linux", env, "broken and absolute symlinks")
	r3.writeFile("broken", link("elsewhere"))
	wantDirty(t, r3, "linux", env, "changed link text")
	// core.symlinks=false: a regular file holding the link text is clean.
	r4 := newRepo(t, map[string]fspec{"l": link("target.txt")})
	os.Remove(filepath.Join(r4.root, "l"))
	os.WriteFile(filepath.Join(r4.root, "l"), []byte("target.txt"), 0o644)
	r4.config("[core]\n\tsymlinks = false\n")
	wantClean(t, r4, "linux", env, "symlinks false")
	os.WriteFile(filepath.Join(r4.root, "l"), []byte("other.txt"), 0o644)
	wantDirty(t, r4, "linux", env, "symlinks false with other text")
}

func TestCleanlinessLineEndings(t *testing.T) {
	env := emptyEnv(t)
	lf := "one\ntwo\n"
	crlf := "one\r\ntwo\r\n"
	r := newRepo(t, map[string]fspec{"t.txt": reg(lf), "bin.dat": reg("a\x00b\r\n")})
	r.writeFile("t.txt", reg(crlf))
	wantDirty(t, r, "linux", env, "CRLF worktree without autocrlf")
	r.config("[core]\n\tautocrlf = true\n")
	wantClean(t, r, "linux", env, "CRLF worktree with autocrlf true")
	r.writeFile("t.txt", reg(lf))
	wantClean(t, r, "linux", env, "raw LF with autocrlf true")
	r.writeFile("bin.dat", reg("a\x00b\n"))
	wantDirty(t, r, "linux", env, "binary content is never converted")
	r.writeFile("bin.dat", reg("a\x00b\r\n"))
	r.writeFile("t.txt", reg("one\rtwo\n"))
	wantDirty(t, r, "linux", env, "a bare CR makes it binary (not converted)")
	// autocrlf=input converts too.
	r2 := newRepo(t, map[string]fspec{"t.txt": reg(lf)})
	r2.config("[core]\n\tautocrlf = input\n")
	r2.writeFile("t.txt", reg(crlf))
	wantClean(t, r2, "linux", env, "autocrlf input")
	// Index already holding CRLF: auto conversion is disabled (Git's
	// safer autocrlf), so the raw worktree bytes must match.
	r3 := newRepo(t, map[string]fspec{"w.txt": reg(crlf)})
	r3.config("[core]\n\tautocrlf = true\n")
	wantClean(t, r3, "linux", env, "CRLF in the index kept")
	r3.writeFile("w.txt", reg(lf))
	wantDirty(t, r3, "linux", env, "LF worktree against a CRLF index blob")
	// Attributes: text forces conversion, -text disables it, eol implies
	// text, text=auto with eol.
	r4 := newRepo(t, map[string]fspec{"a.txt": reg(lf), "b.bin": reg(lf), "c.eol": reg(lf), "d.auto": reg(lf), "e.crlf": reg(lf),
		".gitattributes": reg("*.txt text\n*.bin -text\n*.eol eol=crlf\n*.auto text=auto eol=lf\n*.crlf crlf\n")})
	for _, n := range []string{"a.txt", "b.bin", "c.eol", "d.auto", "e.crlf"} {
		r4.writeFile(n, reg(crlf))
	}
	err := status(t, r4, "linux", env)
	if contract.TransferReason(err) != contract.ReasonDirtySource {
		t.Fatalf("b.bin (-text) should be dirty: %v", err)
	}
	r4.writeFile("b.bin", reg(lf))
	wantClean(t, r4, "linux", env, "text, eol, text=auto and legacy crlf attributes")
	// W1 (normalized content wins): "f text", LF committed and CRLF on
	// disk cleans to the indexed blob, so it is clean, although git
	// status lists it as modified until the index is refreshed.
	r6 := newRepo(t, map[string]fspec{".gitattributes": reg("f text\n"), "f": reg(lf)})
	r6.writeFile("f", reg(crlf))
	wantClean(t, r6, "linux", env, "f text, LF committed, CRLF on disk")
	// ident: the clean form collapses $Id: ... $.
	r5 := newRepo(t, map[string]fspec{"i.c": reg("/* $Id$ */\nx $Id\n$Id: unterminated\n"), ".gitattributes": reg("*.c ident\n")})
	r5.writeFile("i.c", reg("/* $Id: 0123456789abcdef $ */\nx $Id\n$Id: unterminated\n"))
	wantClean(t, r5, "linux", env, "ident expansion")
	r5.writeFile("i.c", reg("/* $Id: broken\nline $ */\nx $Id\n$Id: unterminated\n"))
	wantDirty(t, r5, "linux", env, "ident across a line break is kept")
}

func TestCleanlinessAttributes(t *testing.T) {
	env := emptyEnv(t)
	lf, crlf := "x\n", "x\r\n"
	// info/attributes overrides the tree; nested .gitattributes override
	// the root; a missing worktree .gitattributes falls back to the index.
	r := newRepo(t, map[string]fspec{".gitattributes": reg("*.t text\n"), "sub/.gitattributes": reg("*.t -text\n"), "sub/a.t": reg(lf), "b.t": reg(lf)})
	r.writeFile("b.t", reg(crlf))
	wantClean(t, r, "linux", env, "root text attribute")
	r.writeFile("sub/a.t", reg(crlf))
	wantDirty(t, r, "linux", env, "nested -text wins")
	mustWrite(t, filepath.Join(r.root, ".git/info/attributes"), "*.t text\n")
	wantClean(t, r, "linux", env, "info/attributes wins")
	os.Remove(filepath.Join(r.root, ".git/info/attributes"))
	os.Remove(filepath.Join(r.root, ".gitattributes"))
	// The deletion itself is dirty, but the index fallback still applies
	// to b.t: restore the file to observe only the fallback.
	r.writeFile(".gitattributes", reg("*.t text\n"))
	os.Remove(filepath.Join(r.root, "sub/.gitattributes"))
	os.Symlink("../.gitattributes", filepath.Join(r.root, "sub/.gitattributes"))
	err := status(t, r, "linux", env)
	if contract.TransferReason(err) != contract.ReasonDirtySource {
		t.Fatalf("a symlinked .gitattributes is a type change: %v", err)
	}
	// Global and system attributes are lowest.
	r2 := newRepo(t, map[string]fspec{"g.t": reg(lf)})
	r2.writeFile("g.t", reg(crlf))
	home := tempDir(t)
	mustWrite(t, filepath.Join(home, ".config/git/attributes"), "*.t text\n")
	env2 := envOf(map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1"})
	wantClean(t, r2, "linux", env2, "default global attributes")
	sys := filepath.Join(home, "sysattr")
	mustWrite(t, sys, "*.t -text\n")
	env3 := env2
	env3.SystemAttributes = sys
	wantClean(t, r2, "linux", env3, "global over system")
	mustWrite(t, filepath.Join(home, ".gitconfig"), "[core]\n\tattributesFile = ~/custom\n")
	wantDirty(t, r2, "linux", env3, "configured attributesFile (missing) leaves the system -text")
	// Macros: binary is -text; a user macro with a conversion is refused.
	r3 := newRepo(t, map[string]fspec{"m.t": reg(lf), ".gitattributes": reg("*.t text\n*.t binary\n")})
	r3.writeFile("m.t", reg(crlf))
	wantDirty(t, r3, "linux", env, "binary macro unsets text")
	r4 := newRepo(t, map[string]fspec{"m.t": reg(lf), ".gitattributes": reg("[attr]mine text\n*.t mine\n")})
	wantUnsupported(t, r4, "linux", env, "user macro with a conversion")
	r5 := newRepo(t, map[string]fspec{"m.t": reg(lf), ".gitattributes": reg("[attr]mine diff=x\n*.t mine unknown=1 -delta\n")})
	wantClean(t, r5, "linux", env, "inert user macro and unknown attributes")
	// Filters: configured ones are refused, unconfigured ones are inert.
	r6 := newRepo(t, map[string]fspec{"f.t": reg(lf), ".gitattributes": reg("*.t filter=crypt\n")})
	wantClean(t, r6, "linux", env, "unconfigured filter")
	r6.config("[filter \"crypt\"]\n\tclean = crypt-clean\n")
	wantUnsupported(t, r6, "linux", env, "configured clean filter")
	r7 := newRepo(t, map[string]fspec{"f.t": reg(lf), ".gitattributes": reg("*.t filter=req\n")})
	r7.config("[filter \"req\"]\n\trequired = true\n")
	wantUnsupported(t, r7, "linux", env, "required filter")
	r8 := newRepo(t, map[string]fspec{"f.t": reg(lf), ".gitattributes": reg("*.t working-tree-encoding=UTF-16\n")})
	wantUnsupported(t, r8, "linux", env, "working-tree-encoding")
}

func TestCleanlinessUnicodeKeys(t *testing.T) {
	env := emptyEnv(t)
	nfc, nfd := "café.txt", "café.txt"
	// NFC index path, NFD file on disk (what APFS readdir can return).
	// mk records indexName in the index and creates the working-tree file
	// under diskName afresh (so a normalization-preserving filesystem
	// stores exactly those bytes).
	mk := func(indexName, diskName string) *fixtureRepo {
		r := newRepo(t, map[string]fspec{indexName: reg("x\n")})
		os.Remove(filepath.Join(r.root, indexName))
		if err := os.WriteFile(filepath.Join(r.root, diskName), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := mk(nfc, nfd)
	wantDirty(t, r, "darwin", env, "darwin, precompose unset")
	wantDirty(t, r, "linux", env, "linux, precompose unset")
	r.config("[core]\n\tprecomposeUnicode = true\n")
	wantClean(t, r, "darwin", env, "darwin, NFC index, NFD disk, precompose true")
	wantDirty(t, r, "linux", env, "linux ignores precompose")
	r.config("[core]\n\tprecomposeUnicode = false\n")
	wantDirty(t, r, "darwin", env, "darwin, precompose false")
	// DW1: an NFD index path with an NFD file on disk is a deletion plus
	// an untracked NFC path on darwin with precompose (dirty), byte-exact
	// clean on Linux.
	r2 := mk(nfd, nfd)
	r2.config("[core]\n\tprecomposeUnicode = true\n")
	wantDirty(t, r2, "darwin", env, "darwin, NFD index, NFD disk, precompose true (DW1)")
	wantClean(t, r2, "linux", env, "linux, NFD index, NFD disk")
	r3 := mk(nfc, nfc)
	r3.config("[core]\n\tprecomposeUnicode = true\n")
	wantClean(t, r3, "darwin", env, "identical NFC bytes")
	wantClean(t, r3, "linux", env, "identical NFC bytes on linux")
	// The global setting applies and the repository overrides it.
	home := tempDir(t)
	mustWrite(t, filepath.Join(home, ".gitconfig"), "[core]\n\tprecomposeUnicode = true\n")
	genv := envOf(map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1"})
	r4 := mk(nfc, nfd)
	wantClean(t, r4, "darwin", genv, "global precompose true")
	r4.config("[core]\n\tprecomposeUnicode = false\n")
	wantDirty(t, r4, "darwin", genv, "repository override false")
	// Two raw names with one key are ambiguous, never merged. A
	// normalization-insensitive filesystem (APFS) cannot hold both: there
	// the second write is the same file, an untracked change.
	r5 := newRepo(t, map[string]fspec{"z": reg("z")})
	r5.writeFile(nfc, reg("1"))
	os.WriteFile(filepath.Join(r5.root, nfd), []byte("2"), 0o644)
	r5.config("[core]\n\tprecomposeUnicode = true\n")
	if es, _ := os.ReadDir(r5.root); len(es) == 4 {
		wantUnsupported(t, r5, "darwin", env, "NFC and NFD names on disk")
	} else {
		wantDirty(t, r5, "darwin", env, "one normalized name on disk")
	}
	// Ignore candidates use the key; pattern text keeps its bytes.
	r6 := newRepo(t, map[string]fspec{"z": reg("z"), ".gitignore": reg(nfc + "\n")})
	r6.writeFile(nfd, reg("u"))
	r6.config("[core]\n\tprecomposeUnicode = true\n")
	wantClean(t, r6, "darwin", env, "NFD untracked file matched by an NFC pattern")
	wantDirty(t, r6, "linux", env, "linux: NFC pattern does not match NFD bytes")
	r7 := newRepo(t, map[string]fspec{"z": reg("z"), ".gitignore": reg(nfd + "\n")})
	r7.writeFile(nfd, reg("u"))
	r7.config("[core]\n\tprecomposeUnicode = true\n")
	wantDirty(t, r7, "darwin", env, "an NFD pattern does not match the NFC candidate")
	wantClean(t, r7, "linux", env, "linux: NFD pattern matches NFD bytes")
	// Invalid UTF-8 names keep their bytes (and byte keys), whether or not
	// this filesystem can store them.
	c := statusCfg{precompose: true}
	if c.wtKey("bad\xff"+nfd) != "bad\xff"+nfd || c.wtKey(nfd) != nfc || (statusCfg{}).wtKey(nfd) != nfd {
		t.Fatal("comparison keys")
	}
	if err := os.WriteFile(filepath.Join(tempDir(t), "bad\xff.txt"), nil, 0o644); err == nil {
		r8 := newRepo(t, map[string]fspec{"bad\xff.txt": reg("x")})
		r8.config("[core]\n\tprecomposeUnicode = true\n")
		wantClean(t, r8, "darwin", env, "invalid UTF-8 name")
	}
	// Stored paths are never rewritten.
	idx, _ := os.ReadFile(filepath.Join(r.root, ".git/index"))
	if !bytes.Contains(idx, []byte(nfc)) || bytes.Contains(idx, []byte(nfd)) {
		t.Fatal("the index was rewritten")
	}
}

func TestCleanlinessIgnoreCase(t *testing.T) {
	env := emptyEnv(t)
	r := newRepo(t, map[string]fspec{"README": reg("r\n")})
	// The working tree spells the name in lowercase (a case-preserving
	// rename works on case-sensitive and case-insensitive filesystems).
	os.Rename(filepath.Join(r.root, "README"), filepath.Join(r.root, "readme"))
	names, _ := os.ReadDir(r.root)
	if len(names) != 2 || names[1].Name() != "readme" {
		t.Fatalf("readdir %v", names)
	}
	wantDirty(t, r, "linux", env, "case-sensitive comparison")
	r.config("[core]\n\tignorecase = true\n")
	wantClean(t, r, "linux", env, "core.ignoreCase relates readme to README")
	_, err := os.Lstat(filepath.Join(r.root, "README"))
	caseInsensitive := err == nil
	os.WriteFile(filepath.Join(r.root, "README"), []byte("other\n"), 0o644)
	if caseInsensitive {
		// The same file under another spelling: its content changed.
		wantDirty(t, r, "linux", env, "edited through another spelling")
	} else {
		wantUnsupported(t, r, "linux", env, "two names differing only by case under ignoreCase")
	}
	r2 := newRepo(t, map[string]fspec{"A": reg("1"), "a": reg("2")})
	r2.config("[core]\n\tignorecase = true\n")
	wantUnsupported(t, r2, "linux", env, "tracked paths differing only by case under ignoreCase")
}

func TestCleanlinessExcludesSources(t *testing.T) {
	r := newRepo(t, baseFiles())
	r.writeFile("g.global", reg("x"))
	r.writeFile("i.info", reg("x"))
	home := tempDir(t)
	env := envOf(map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1"})
	wantDirty(t, r, "linux", env, "no excludes")
	mustWrite(t, filepath.Join(r.root, ".git/info/exclude"), "*.info\n")
	mustWrite(t, filepath.Join(home, ".config/git/ignore"), "*.global\n")
	wantClean(t, r, "linux", env, "default XDG ignore and info/exclude")
	xdg := tempDir(t)
	env2 := envOf(map[string]string{"HOME": home, "XDG_CONFIG_HOME": xdg, "GIT_CONFIG_NOSYSTEM": "1"})
	wantDirty(t, r, "linux", env2, "XDG_CONFIG_HOME moves the default file")
	mustWrite(t, filepath.Join(xdg, "git/ignore"), "*.global\n")
	wantClean(t, r, "linux", env2, "XDG ignore")
	mustWrite(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = ~/my-ignore\n")
	wantDirty(t, r, "linux", env2, "configured excludesFile (missing) replaces the default")
	mustWrite(t, filepath.Join(home, "my-ignore"), "*.global\n")
	wantClean(t, r, "linux", env2, "tilde excludesFile")
	mustWrite(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile =\n")
	wantDirty(t, r, "linux", env2, "empty excludesFile disables the global file")
	// Local rules win over global ones.
	mustWrite(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = ~/my-ignore\n")
	mustWrite(t, filepath.Join(r.root, ".git/info/exclude"), "*.info\n!keep.global\n")
	r.writeFile("keep.global", reg("x"))
	wantDirty(t, r, "linux", env2, "info/exclude negation beats the global file")
	os.Remove(filepath.Join(r.root, "keep.global"))
	// An unreadable configured file fails, never "no rules".
	os.Remove(filepath.Join(home, "my-ignore"))
	os.Mkdir(filepath.Join(home, "my-ignore"), 0o755)
	if err := status(t, r, "linux", env2); contract.TransferReason(err) != contract.ReasonUnsupportedRepository {
		t.Fatalf("unreadable excludes file: %v", err)
	}
	// A symlinked .gitignore is not read as rules.
	r2 := newRepo(t, map[string]fspec{"a": reg("a"), ".gitignore": link("rules")})
	r2.writeFile("rules", reg("*.x\n"))
	r2.writeFile("f.x", reg("x"))
	r2.writeFile(".git/info/exclude", reg("rules\n"))
	wantDirty(t, r2, "linux", emptyEnv(t), "symlinked .gitignore not followed")
}

func TestCleanlinessNestedRepository(t *testing.T) {
	env := emptyEnv(t)
	r := newRepo(t, map[string]fspec{".gitignore": reg("ignored/\n"), "a": reg("a")})
	newRepoAt(t, filepath.Join(r.root, "ignored", "inner"), map[string]fspec{"x": reg("x")})
	wantClean(t, r, "linux", env, "nested repository in an ignored directory")
	newRepoAt(t, filepath.Join(r.root, "vendor", "inner"), map[string]fspec{"x": reg("x")})
	wantUnsupported(t, r, "linux", env, "nested repository in a nonignored directory")
}

// The check writes nothing: index, config and HEAD bytes and times are
// unchanged; the manifest detects later edits, additions, removals and
// HEAD, index and config changes.
func TestCleanlinessReadOnlyAndManifest(t *testing.T) {
	env := emptyEnv(t)
	r := newRepo(t, baseFiles())
	stamp := func() map[string]string {
		out := map[string]string{}
		for _, p := range []string{".git/index", ".git/config", ".git/HEAD", ".git/refs/heads/main"} {
			b, _ := os.ReadFile(filepath.Join(r.root, p))
			fi, _ := os.Stat(filepath.Join(r.root, p))
			out[p] = string(b) + fi.ModTime().String()
		}
		return out
	}
	before := stamp()
	rp, err := openRepo(context.Background(), env, r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer rp.close()
	man, err := checkClean(context.Background(), fastDeps(), rp, env, "linux")
	if err != nil {
		t.Fatal(err)
	}
	after := stamp()
	for k := range before {
		if before[k] != after[k] {
			t.Fatalf("%s changed", k)
		}
	}
	verify := func() error { return man.verify(context.Background(), rp.rootFD) }
	if err := verify(); err != nil {
		t.Fatalf("unchanged source: %v", err)
	}
	for _, c := range []struct {
		what         string
		change, undo func()
	}{
		{"content edit", func() {
			time.Sleep(10 * time.Millisecond)
			r.writeFile("a.txt", reg("alpha!\n"))
		}, func() { r.writeFile("a.txt", reg("alpha\n")) }},
		{"new file", func() { r.writeFile("dir/new", reg("n")) }, func() { os.Remove(filepath.Join(r.root, "dir/new")) }},
		{"HEAD", func() { mustWrite(t, filepath.Join(r.root, ".git/HEAD"), r.head.String()+"\n") }, nil},
		{"config", func() { r.config("[core]\n\tautocrlf = true\n") }, nil},
		{"index", func() { r.writeIndex(map[string]fspec{"a.txt": reg("alpha\n")}) }, nil},
	} {
		c.change()
		if err := verify(); err != errChanged {
			t.Fatalf("%s: %v", c.what, err)
		}
		if c.undo == nil {
			break
		}
		c.undo()
	}
	// A stat-only index refresh (same entries) is no change.
	if indexDigest(filepath.Join(r.root, "nope")) != [32]byte{} {
		t.Fatal("absent index digest")
	}
}

// indexWithFlags patches a v2 index's first entry flags (assume-valid)
// and fixes the trailer.
func indexWithFlags(t *testing.T, b []byte, set uint16) []byte {
	t.Helper()
	out := append([]byte(nil), b[:len(b)-20]...)
	off := 12 + 60
	flags := uint16(out[off])<<8 | uint16(out[off+1])
	flags |= set
	out[off], out[off+1] = byte(flags>>8), byte(flags)
	sum := sha1.Sum(out)
	return append(out, sum[:]...)
}
