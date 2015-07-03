package shard

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/pachyderm/pachyderm/src/btrfs"
	"github.com/pachyderm/pachyderm/src/log"
	"github.com/pachyderm/pachyderm/src/pipeline"
	"github.com/pachyderm/pachyderm/src/route"
	"github.com/satori/go.uuid"
)

const (
	pipelineDir = "pipeline"
)

type Shard struct {
	url                string
	dataRepo, compRepo string
	pipelinePrefix     string
	shard, modulos     uint64
	shardStr           string
	runners            map[string]*pipeline.Runner
	guard              sync.Mutex
}

func ShardFromArgs() (*Shard, error) {
	shard, modulos, err := route.ParseShard(os.Args[1])
	if err != nil {
		return nil, err
	}
	return &Shard{
		url:            "http://" + os.Args[2],
		dataRepo:       "data-" + os.Args[1],
		compRepo:       "comp-" + os.Args[1],
		pipelinePrefix: "pipe-" + os.Args[1],
		shard:          shard,
		modulos:        modulos,
		shardStr:       os.Args[1],
		runners:        make(map[string]*pipeline.Runner),
	}, nil
}

func NewShard(dataRepo, compRepo, pipelinePrefix string, shard, modulos uint64) *Shard {
	return &Shard{
		dataRepo:       dataRepo,
		compRepo:       compRepo,
		pipelinePrefix: pipelinePrefix,
		shard:          shard,
		modulos:        modulos,
		shardStr:       fmt.Sprint(shard, "-", modulos),
		runners:        make(map[string]*pipeline.Runner),
	}
}

func (s *Shard) EnsureRepos() error {
	if err := btrfs.Ensure(s.dataRepo); err != nil {
		return err
	}
	if err := btrfs.Ensure(s.compRepo); err != nil {
		return err
	}
	return nil
}

// ShardMux creates a multiplexer for a Shard writing to the passed in FS.
func (s *Shard) ShardMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/branch", s.branchHandler)
	mux.HandleFunc("/commit", s.commitHandler)
	mux.HandleFunc("/file/", s.fileHandler)
	mux.HandleFunc("/pipeline/", s.pipelineHandler)
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "pong\n") })
	mux.HandleFunc("/pull", s.pullHandler)

	return mux
}

// RunServer runs a shard server listening on port 80.
func (s *Shard) RunServer() error {
	return http.ListenAndServe(":80", s.ShardMux())
}

// FileHandler is the core route for modifying the contents of the fileystem.
func (s *Shard) fileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" || r.Method == "DELETE" || r.Method == "PUT" {
		genericFileHandler(path.Join(s.dataRepo, branchParam(r)), w, r)
	} else if r.Method == "GET" {
		genericFileHandler(path.Join(s.dataRepo, commitParam(r)), w, r)
	} else {
		http.Error(w, "Invalid method.", 405)
	}
}

// CommitHandler creates a snapshot of outstanding changes.
func (s *Shard) commitHandler(w http.ResponseWriter, r *http.Request) {
	url := strings.Split(r.URL.Path, "/")
	// url looks like [, commit, <commit>, file, <file>]
	if len(url) > 3 && url[3] == "file" {
		genericFileHandler(path.Join(s.dataRepo, url[2]), w, r)
		return
	}
	if r.Method == "GET" {
		encoder := json.NewEncoder(w)
		btrfs.Commits(s.dataRepo, "", btrfs.Desc, func(name string) error {
			isReadOnly, err := btrfs.IsCommit(path.Join(s.dataRepo, name))
			if err != nil {
				return err
			}
			if isReadOnly {
				fi, err := btrfs.Stat(path.Join(s.dataRepo, name))
				if err != nil {
					return err
				}
				err = encoder.Encode(CommitMsg{Name: fi.Name(), TStamp: fi.ModTime().Format("2006-01-02T15:04:05.999999-07:00")})
				if err != nil {
					return err
				}
			}
			return nil
		})
	} else if r.Method == "POST" && r.ContentLength == 0 {
		// Create a commit from local data
		var commit string
		if commit = r.URL.Query().Get("commit"); commit == "" {
			commit = uuid.NewV4().String()
		}
		err := btrfs.Commit(s.dataRepo, commit, branchParam(r))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		// We lock the guard so that we can remove the oldRunner from the map
		// and add the newRunner in.
		s.guard.Lock()
		oldRunner, ok := s.runners[branchParam(r)]
		newRunner := pipeline.NewRunner("pipeline", s.dataRepo, s.pipelinePrefix, commit, branchParam(r), s.shardStr)
		s.runners[branchParam(r)] = newRunner
		s.guard.Unlock()
		go func() {
			// cancel oldRunner if it exists
			if ok {
				err := oldRunner.Cancel()
				if err != nil {
					log.Print(err)
				}
			}
			err := newRunner.Run()
			if err != nil {
				log.Print(err)
			}
		}()
		go s.SyncToPeers()
		fmt.Fprintf(w, "%s\n", commit)
	} else if r.Method == "POST" {
		// Commit being pushed via a diff
		replica := btrfs.NewLocalReplica(s.dataRepo)
		if err := replica.Push(r.Body); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	} else {
		http.Error(w, "Unsupported method.", http.StatusMethodNotAllowed)
		log.Printf("Unsupported method %s in request to %s.", r.Method, r.URL.String())
		return
	}
}

// BranchHandler creates a new branch from commit.
func (s *Shard) branchHandler(w http.ResponseWriter, r *http.Request) {
	url := strings.Split(r.URL.Path, "/")
	// url looks like [, commit, <commit>, file, <file>]
	if len(url) > 3 && url[3] == "file" {
		genericFileHandler(path.Join(s.dataRepo, url[2]), w, r)
		return
	}
	if r.Method == "GET" {
		encoder := json.NewEncoder(w)
		btrfs.Commits(s.dataRepo, "", btrfs.Desc, func(name string) error {
			isReadOnly, err := btrfs.IsCommit(path.Join(s.dataRepo, name))
			if err != nil {
				return err
			}
			if !isReadOnly {
				fi, err := btrfs.Stat(path.Join(s.dataRepo, name))
				if err != nil {
					return err
				}
				err = encoder.Encode(BranchMsg{Name: fi.Name(), TStamp: fi.ModTime().Format("2006-01-02T15:04:05.999999-07:00")})
				if err != nil {
					return err
				}
			}
			return nil
		})
	} else if r.Method == "POST" {
		if err := btrfs.Branch(s.dataRepo, commitParam(r), branchParam(r)); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "Created branch. (%s) -> %s.\n", commitParam(r), branchParam(r))
	} else {
		http.Error(w, "Invalid method.", 405)
		log.Printf("Invalid method %s.", r.Method)
		return
	}
}

func (s *Shard) pipelineHandler(w http.ResponseWriter, r *http.Request) {
	url := strings.Split(r.URL.Path, "/")
	if r.Method == "GET" && len(url) > 3 && url[3] == "file" {
		// First wait for the commit to show up
		err := pipeline.WaitPipeline(s.pipelinePrefix, url[2], commitParam(r))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// url looks like [, pipeline, <pipeline>, file, <file>]
		genericFileHandler(path.Join(s.pipelinePrefix, url[2], commitParam(r)), w, r)
		return
	} else if r.Method == "POST" {
		r.URL.Path = path.Join("/file", pipelineDir, url[2])
		genericFileHandler(path.Join(s.dataRepo, branchParam(r)), w, r)
	} else {
		http.Error(w, "Invalid method.", 405)
		log.Printf("Invalid method %s.", r.Method)
		return
	}
}

func (s *Shard) pullHandler(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	mpw := multipart.NewWriter(w)
	defer mpw.Close()
	cb := NewMultipartReplica(mpw)
	w.Header().Add("Boundary", mpw.Boundary())
	localReplica := btrfs.NewLocalReplica(s.dataRepo)
	err := localReplica.Pull(from, cb)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
}

// genericFileHandler serves files from fs. It's used after branch and commit
// info have already been extracted and ignores those aspects of the URL.
func genericFileHandler(fs string, w http.ResponseWriter, r *http.Request) {
	url := strings.Split(r.URL.Path, "/")
	// url looks like: /foo/bar/.../file/<file>
	fileStart := indexOf(url, "file") + 1
	// file is the path in the filesystem we're getting
	file := path.Join(append([]string{fs}, url[fileStart:]...)...)

	if r.Method == "GET" {
		files, err := btrfs.Glob(file)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		switch len(files) {
		case 0:
			http.Error(w, "404 page not found", 404)
			return
		case 1:
			http.ServeFile(w, r, btrfs.FilePath(files[0]))
		default:
			writer := multipart.NewWriter(w)
			defer writer.Close()
			w.Header().Add("Boundary", writer.Boundary())
			for _, file := range files {
				info, err := btrfs.Stat(file)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				if info.IsDir() {
					// We don't do anything with directories.
					continue
				}
				name := strings.TrimPrefix(file, "/"+fs+"/")
				if shardParam(r) != "" {
					// We have a shard param, check if the file matches the shard.
					match, err := route.Match(name, shardParam(r))
					if err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
					if !match {
						continue
					}
				}
				fWriter, err := writer.CreateFormFile(name, name)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				err = rawCat(fWriter, file)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
		}
	} else if r.Method == "POST" {
		btrfs.MkdirAll(path.Dir(file))
		size, err := btrfs.CreateFromReader(file, r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "Created %s, size: %d.\n", path.Join(url[fileStart:]...), size)
	} else if r.Method == "PUT" {
		btrfs.MkdirAll(path.Dir(file))
		size, err := btrfs.CopyFile(file, r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "Created %s, size: %d.\n", path.Join(url[fileStart:]...), size)
	} else if r.Method == "DELETE" {
		if err := btrfs.Remove(file); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "Deleted %s.\n", file)
	}
}

func commitParam(r *http.Request) string {
	if p := r.URL.Query().Get("commit"); p != "" {
		return p
	}
	return "master"
}

func branchParam(r *http.Request) string {
	if p := r.URL.Query().Get("branch"); p != "" {
		return p
	}
	return "master"
}

func shardParam(r *http.Request) string {
	return r.URL.Query().Get("shard")
}

func hasBranch(r *http.Request) bool {
	return (r.URL.Query().Get("branch") == "")
}

func materializeParam(r *http.Request) string {
	if _, ok := r.URL.Query()["run"]; ok {
		return "true"
	}
	return "false"
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

func rawCat(w io.Writer, name string) error {
	f, err := btrfs.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	return nil
}
