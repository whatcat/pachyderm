package fileset

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/pachyderm/pachyderm/src/server/pkg/dbutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/obj"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/chunk"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset/index"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/tracker"
	"github.com/pachyderm/pachyderm/src/server/pkg/tar"
)

// WithTestStorage constructs a local storage instance for testing during the lifetime of
// the callback.
func WithTestStorage(t testing.TB, f func(*Storage) error) {
	dbutil.WithTestDB(t, func(db *sqlx.DB) {
		chunk.SetupPostgresStore(db)
		tracker.PGTrackerApplySchema(db)
		SetupPostgresStore(db)
		tr := tracker.NewPGTracker(db)
		obj.WithLocalClient(func(objC obj.Client) error {
			chunkStorage := chunk.NewStorage(objC, chunk.NewPostgresStore(db), tr)
			return f(NewStorage(NewPostgresStore(db), tr, chunkStorage))
		})
	})
}

// CopyFiles copies files from a file set to a file set writer.
func CopyFiles(ctx context.Context, w *Writer, fs FileSet) error {
	return fs.Iterate(ctx, func(f File) error {
		return w.Copy(f)
	})
}

// WriteTarEntry writes an tar entry for f to w
func WriteTarEntry(w io.Writer, f File) error {
	h, err := f.Header()
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if err := f.Content(tw); err != nil {
		return err
	}
	return tw.Flush()
}

// WriteTarStream writes an entire tar stream to w
// It will contain an entry for each File in fs
func WriteTarStream(ctx context.Context, w io.Writer, fs FileSet) error {
	if err := fs.Iterate(ctx, func(f File) error {
		return WriteTarEntry(w, f)
	}); err != nil {
		return err
	}
	return tar.NewWriter(w).Close()
}

// WithTarFileWriter wraps a file writer with a tar file writer.
func WithTarFileWriter(fw *FileWriter, hdr *tar.Header, cb func(*TarFileWriter) error) error {
	tw := tar.NewWriter(fw)
	fw.Append(headerTag)
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if err := cb(&TarFileWriter{
		FileWriter: fw,
		tw:         tw,
	}); err != nil {
		return err
	}
	fw.Append(paddingTag)
	return tw.Flush()
}

// TarFileWriter is used for writing a tar file to a file writer.
type TarFileWriter struct {
	*FileWriter
	tw *tar.Writer
}

// Copy copies a set of data ops to the tar file writer.
func (tfw *TarFileWriter) Copy(dataOps []*index.DataOp) error {
	for _, dataOp := range dataOps {
		for _, dataRef := range dataOp.DataRefs {
			if err := tfw.tw.Skip(dataRef.SizeBytes); err != nil {
				return err
			}
		}
	}
	return tfw.FileWriter.Copy(dataOps)
}

func (tfw *TarFileWriter) Write(data []byte) (int, error) {
	return tfw.tw.Write(data)
}

// CleanTarPath ensures that the path is in the canonical format for tar header names.
// This includes ensuring a prepending /'s and ensure directory paths
// have a trailing slash.
func CleanTarPath(x string, isDir bool) string {
	y := "/" + strings.Trim(x, "/")
	if isDir && !IsDir(y) {
		y += "/"
	}
	return y
}

// IsCleanTarPath determines if the path is a valid tar path.
func IsCleanTarPath(x string, isDir bool) bool {
	y := CleanTarPath(x, isDir)
	return y == x
}

// IsDir determines if a path is for a directory.
func IsDir(p string) bool {
	return strings.HasSuffix(p, "/")
}

// DirUpperBound returns the immediate next path after a directory in a lexicographical ordering.
func DirUpperBound(p string) string {
	if !IsDir(p) {
		panic(fmt.Sprintf("%v is not a directory path", p))
	}
	return strings.TrimRight(p, "/") + "0"
}

// Iterator provides functionality for imperative iteration over a file set.
type Iterator struct {
	peek     File
	fileChan chan File
	errChan  chan error
}

// NewIterator creates a new iterator.
func NewIterator(ctx context.Context, fs FileSet) *Iterator {
	fileChan := make(chan File)
	errChan := make(chan error, 1)
	go func() {
		if err := fs.Iterate(ctx, func(f File) error {
			fileChan <- f
			return nil
		}); err != nil {
			errChan <- err
			return
		}
		close(fileChan)
	}()
	return &Iterator{
		fileChan: fileChan,
		errChan:  errChan,
	}
}

// Peek returns the next file without progressing the iterator.
func (i *Iterator) Peek() (File, error) {
	if i.peek != nil {
		return i.peek, nil
	}
	var err error
	i.peek, err = i.Next()
	return i.peek, err
}

// Next returns the next file and progresses the iterator.
func (i *Iterator) Next() (File, error) {
	if i.peek != nil {
		tmp := i.peek
		i.peek = nil
		return tmp, nil
	}
	select {
	case file, more := <-i.fileChan:
		if !more {
			return nil, io.EOF
		}
		return file, nil
	case err := <-i.errChan:
		return nil, err
	}
}

// TODO: Might want to tighten up the invariants surrounding the extracting of header, content, and padding data ops.
// Right now, the general behavior is that at read time the indexes will always have header, content, and padding data ops
// except in the case of a merge reader which will at least have the header and content tags (the padding will need to be
// generated by a tar writer when the merge is performed).
func getHeaderDataOp(dataOps []*index.DataOp) *index.DataOp {
	return dataOps[0]
}

func getContentDataOps(dataOps []*index.DataOp) []*index.DataOp {
	if dataOps[0].Tag == headerTag {
		dataOps = dataOps[1:]
	}
	if dataOps[len(dataOps)-1].Tag == paddingTag {
		dataOps = dataOps[:len(dataOps)-1]
	}
	return dataOps
}

func getDataRefs(dataOps []*index.DataOp) []*chunk.DataRef {
	var dataRefs []*chunk.DataRef
	for _, dataOp := range dataOps {
		dataRefs = append(dataRefs, dataOp.DataRefs...)
	}
	return dataRefs
}
