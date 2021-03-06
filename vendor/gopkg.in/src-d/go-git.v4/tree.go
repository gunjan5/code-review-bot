package git

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"gopkg.in/src-d/go-git.v4/core"
)

const (
	maxTreeDepth      = 1024
	startingStackSize = 8
	submoduleMode     = 0160000
	directoryMode     = 0040000
)

// New errors defined by this package.
var (
	ErrMaxTreeDepth = errors.New("maximum tree depth exceeded")
	ErrFileNotFound = errors.New("file not found")
)

// Tree is basically like a directory - it references a bunch of other trees
// and/or blobs (i.e. files and sub-directories)
type Tree struct {
	Entries []TreeEntry
	Hash    core.Hash

	r *Repository
	m map[string]*TreeEntry
}

// TreeEntry represents a file
type TreeEntry struct {
	Name string
	Mode os.FileMode
	Hash core.Hash
}

// File returns the hash of the file identified by the `path` argument.
// The path is interpreted as relative to the tree receiver.
func (t *Tree) File(path string) (*File, error) {
	e, err := t.findEntry(path)
	if err != nil {
		return nil, ErrFileNotFound
	}

	obj, err := t.r.s.ObjectStorage().Get(core.BlobObject, e.Hash)
	if err != nil {
		return nil, err
	}

	blob := &Blob{}
	blob.Decode(obj)

	return NewFile(path, e.Mode, blob), nil
}

func (t *Tree) findEntry(path string) (*TreeEntry, error) {
	pathParts := strings.Split(path, "/")

	var tree *Tree
	var err error
	for tree = t; len(pathParts) > 1; pathParts = pathParts[1:] {
		if tree, err = tree.dir(pathParts[0]); err != nil {
			return nil, err
		}
	}

	return tree.entry(pathParts[0])
}

var errDirNotFound = errors.New("directory not found")

func (t *Tree) dir(baseName string) (*Tree, error) {
	entry, err := t.entry(baseName)
	if err != nil {
		return nil, errDirNotFound
	}

	obj, err := t.r.s.ObjectStorage().Get(core.TreeObject, entry.Hash)
	if err != nil {
		return nil, err
	}

	tree := &Tree{r: t.r}
	tree.Decode(obj)

	return tree, nil
}

var errEntryNotFound = errors.New("entry not found")

func (t *Tree) entry(baseName string) (*TreeEntry, error) {
	if t.m == nil {
		t.buildMap()
	}
	entry, ok := t.m[baseName]
	if !ok {
		return nil, errEntryNotFound
	}

	return entry, nil
}

// Files returns a FileIter allowing to iterate over the Tree
func (t *Tree) Files() *FileIter {
	return NewFileIter(t.r, t)
}

// ID returns the object ID of the tree. The returned value will always match
// the current value of Tree.Hash.
//
// ID is present to fulfill the Object interface.
func (t *Tree) ID() core.Hash {
	return t.Hash
}

// Type returns the type of object. It always returns core.TreeObject.
func (t *Tree) Type() core.ObjectType {
	return core.TreeObject
}

// Decode transform an core.Object into a Tree struct
func (t *Tree) Decode(o core.Object) (err error) {
	if o.Type() != core.TreeObject {
		return ErrUnsupportedObject
	}

	t.Hash = o.Hash()
	if o.Size() == 0 {
		return nil
	}

	t.Entries = nil
	t.m = nil

	reader, err := o.Reader()
	if err != nil {
		return err
	}
	defer checkClose(reader, &err)

	r := bufio.NewReader(reader)
	for {
		mode, err := r.ReadString(' ')
		if err != nil {
			if err == io.EOF {
				break
			}

			return err
		}

		fm, err := t.decodeFileMode(mode[:len(mode)-1])
		if err != nil && err != io.EOF {
			return err
		}

		name, err := r.ReadString(0)
		if err != nil && err != io.EOF {
			return err
		}

		var hash core.Hash
		if _, err = io.ReadFull(r, hash[:]); err != nil {
			return err
		}

		baseName := name[:len(name)-1]
		t.Entries = append(t.Entries, TreeEntry{
			Hash: hash,
			Mode: fm,
			Name: baseName,
		})
	}

	return nil
}

func (t *Tree) decodeFileMode(mode string) (os.FileMode, error) {
	fm, err := strconv.ParseInt(mode, 8, 32)
	if err != nil && err != io.EOF {
		return 0, err
	}

	m := os.FileMode(fm)
	switch fm {
	case 0040000: //tree
		m = m | os.ModeDir
	case 0120000: //symlink
		m = m | os.ModeSymlink
	}

	return m, nil
}

// Encode transforms a Tree into a core.Object.
func (t *Tree) Encode(o core.Object) error {
	o.SetType(core.TreeObject)
	w, err := o.Writer()
	if err != nil {
		return err
	}

	var size int
	defer checkClose(w, &err)
	for _, entry := range t.Entries {
		n, err := fmt.Fprintf(w, "%o %s", entry.Mode, entry.Name)
		if err != nil {
			return err
		}

		size += n
		n, err = w.Write([]byte{0x00})
		if err != nil {
			return err
		}

		size += n
		n, err = w.Write([]byte(entry.Hash[:]))
		if err != nil {
			return err
		}
		size += n
	}

	o.SetSize(int64(size))
	return err
}

func (t *Tree) buildMap() {
	t.m = make(map[string]*TreeEntry)
	for i := 0; i < len(t.Entries); i++ {
		t.m[t.Entries[i].Name] = &t.Entries[i]
	}
}

// treeEntryIter facilitates iterating through the TreeEntry objects in a Tree.
type treeEntryIter struct {
	t   *Tree
	pos int
}

func (iter *treeEntryIter) Next() (TreeEntry, error) {
	if iter.pos >= len(iter.t.Entries) {
		return TreeEntry{}, io.EOF
	}
	iter.pos++
	return iter.t.Entries[iter.pos-1], nil
}

// TreeWalker provides a means of walking through all of the entries in a Tree.
type TreeIter struct {
	stack     []treeEntryIter
	base      string
	recursive bool

	r *Repository
	t *Tree
}

// NewTreeIter returns a new TreeIter for the given repository and tree.
//
// It is the caller's responsibility to call Close() when finished with the
// tree walker.
func NewTreeIter(r *Repository, t *Tree, recursive bool) *TreeIter {
	stack := make([]treeEntryIter, 0, startingStackSize)
	stack = append(stack, treeEntryIter{t, 0})

	return &TreeIter{
		stack:     stack,
		recursive: recursive,

		r: r,
		t: t,
	}
}

// Next returns the next object from the tree. Objects are returned in order
// and subtrees are included. After the last object has been returned further
// calls to Next() will return io.EOF.
//
// In the current implementation any objects which cannot be found in the
// underlying repository will be skipped automatically. It is possible that this
// may change in future versions.
func (w *TreeIter) Next() (name string, entry TreeEntry, err error) {
	var obj Object
	for {
		current := len(w.stack) - 1
		if current < 0 {
			// Nothing left on the stack so we're finished
			err = io.EOF
			return
		}

		if current > maxTreeDepth {
			// We're probably following bad data or some self-referencing tree
			err = ErrMaxTreeDepth
			return
		}

		entry, err = w.stack[current].Next()
		if err == io.EOF {
			// Finished with the current tree, move back up to the parent
			w.stack = w.stack[:current]
			w.base, _ = path.Split(w.base)
			w.base = path.Clean(w.base) // Remove trailing slash
			continue
		}

		if err != nil {
			return
		}

		if entry.Mode == submoduleMode {
			err = nil
			continue
		}

		if entry.Mode.IsDir() {
			obj, err = w.r.Tree(entry.Hash)
		}

		name = path.Join(w.base, entry.Name)

		if err != nil {
			err = io.EOF
			return
		}

		break
	}

	if !w.recursive {
		return
	}

	if t, ok := obj.(*Tree); ok {
		w.stack = append(w.stack, treeEntryIter{t, 0})
		w.base = path.Join(w.base, entry.Name)
	}

	return
}

// Tree returns the tree that the tree walker most recently operated on.
func (w *TreeIter) Tree() *Tree {
	current := len(w.stack) - 1
	if w.stack[current].pos == 0 {
		current--
	}

	if current < 0 {
		return nil
	}

	return w.stack[current].t
}

// Close releases any resources used by the TreeWalker.
func (w *TreeIter) Close() {
	w.stack = nil
}
