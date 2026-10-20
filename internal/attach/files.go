package attach

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
)

// MaxReadFile bounds what ReadFile returns of a file.
const MaxReadFile = 256 << 10

// readableDirs are the parts of a session folder a viewer may read.
var readableDirs = []string{"inputs", "workspace", "output"}

// ServeFiles lets viewers read the files of the session folder dir, in
// inputs/, workspace/ and output/ only.
func (s *Server) ServeFiles(dir string) { s.dir = dir }

// ReadFile implements the ReadFile call.
func (s *Server) ReadFile(ctx context.Context, req *attachv0.ReadFileRequest) (*attachv0.ReadFileResponse, error) {
	if err := s.checkSession(ctx); err != nil {
		return nil, err
	}
	if s.dir == "" {
		return nil, status.Error(codes.Unimplemented, "this box serves no files")
	}
	return ReadFile(s.dir, req)
}

// ReadFile reads a file of the session folder dir as the ReadFile call
// does, for a viewer on the same machine once the box has gone.
func ReadFile(dir string, req *attachv0.ReadFileRequest) (*attachv0.ReadFileResponse, error) {
	if req.GetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "no path")
	}
	path := req.GetPath()
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, "workspace", path)
	}
	// Resolve symbolic links first, so none leads out of the readable
	// directories.
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "%s doesn't exist", req.GetPath())
	}
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	name, base, inside, ok := readable(dir, real)
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "%s is outside inputs/, workspace/ and output/", req.GetPath())
	}
	// Open within the readable directory, which a symbolic link swapped in
	// since the check can't lead out of: the agent controls workspace/.
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(inside)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "%s: %v", req.GetPath(), err)
	}
	defer func() { _ = f.Close() }()
	rel := filepath.ToSlash(filepath.Join(name, inside))
	fi, err := f.Stat()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, status.Errorf(codes.InvalidArgument, "%s isn't a file", req.GetPath())
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxReadFile))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	resp := &attachv0.ReadFileResponse{Path: rel, Size: fi.Size(), Truncated: fi.Size() > int64(len(data))}
	if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
		resp.Binary = true
	} else {
		resp.Content = data
	}
	return resp, nil
}

// readable reports whether a resolved path is in a readable directory of
// the session folder dir, and returns that directory's name, the
// directory resolved, and the path inside it.
func readable(dir, real string) (name, base, inside string, ok bool) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", "", false
	}
	for _, d := range readableDirs {
		base, err := filepath.EvalSymlinks(filepath.Join(root, d))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(base, real)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return d, base, rel, true
	}
	return "", "", "", false
}
