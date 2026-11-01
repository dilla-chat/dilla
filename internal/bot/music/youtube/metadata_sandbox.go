package youtube

import (
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// IsolatedMetadataRunner executes one metadata command in the same network
// namespace and public-YouTube-only broker used by PCMStarter.
type IsolatedMetadataRunner struct {
	Bubblewrap string
	Python     string
	Node       string
	Timeout    time.Duration
}

func (r IsolatedMetadataRunner) Run(ctx context.Context, executable string, args []string, outputLimit int) ([]byte, error) {
	if ctx == nil || outputLimit < 1 || outputLimit > MaxMetadataBytes || len(args) > 128 {
		return nil, ErrExtractFailed
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return nil, ErrExtractFailed
		}
	}
	bwrap, err := r.executable(r.Bubblewrap, "bwrap")
	if err != nil {
		return nil, ErrSandboxUnavailable
	}
	python, err := r.executable(r.Python, "python3")
	if err != nil {
		return nil, ErrSandboxUnavailable
	}
	executable, err = resolveExecutable(executable, "metadata extractor")
	if err != nil {
		return nil, ErrExtractFailed
	}
	tools := []string{executable}
	var configuredNode string
	if r.Node != "" {
		configuredNode, err = resolveExecutable(r.Node, "node")
		if err != nil {
			return nil, ErrExtractFailed
		}
		tools = append(tools, configuredNode)
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--js-runtimes" {
			if !strings.HasPrefix(args[i+1], "node:") || configuredNode == "" || strings.TrimPrefix(args[i+1], "node:") != r.Node {
				return nil, ErrExtractFailed
			}
		}
	}
	timeout := r.Timeout
	if timeout <= 0 || timeout > DefaultProcessTimeout {
		timeout = DefaultProcessTimeout
	}
	workerArgs := append([]string{"exec", strconv.Itoa(outputLimit), executable}, args...)
	process, err := launchNetworkWorker(ctx, bwrap, python, tools, workerArgs, min(timeout, defaultSandboxStartupTimeout), timeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = process.Close() }()
	output, err := io.ReadAll(io.LimitReader(process.stdout, int64(outputLimit)+1))
	if err != nil {
		_ = process.Close()
		return nil, process.failure(ErrExtractFailed)
	}
	if len(output) > outputLimit {
		_ = process.Close()
		return nil, process.failure(ErrMetadataTooLarge)
	}
	<-process.done
	if process.ctxErr != nil {
		return nil, process.failure(process.ctxErr)
	}
	if process.exitCode == 75 {
		return nil, process.failure(ErrMetadataTooLarge)
	}
	if process.err != nil {
		return nil, process.failure(ErrExtractFailed)
	}
	return output, nil
}

func (r IsolatedMetadataRunner) executable(path, name string) (string, error) {
	if path == "" {
		var err error
		path, err = exec.LookPath(name)
		if err != nil {
			return "", err
		}
	}
	return resolveExecutable(path, name)
}

func (IsolatedMetadataRunner) youtubeSandboxedRunner() {}

// NewMetadataResolver validates its tool paths and selects the package's
// bubblewrap and public-YouTube egress broker implementation.
func NewMetadataResolver(ytdlp, node, bubblewrap, python string) (MetadataResolver, error) {
	var err error
	if ytdlp, err = resolveExecutable(ytdlp, "yt-dlp"); err != nil {
		return MetadataResolver{}, err
	}
	if node, err = resolveExecutable(node, "node"); err != nil {
		return MetadataResolver{}, err
	}
	if bubblewrap, err = resolveExecutable(bubblewrap, "bwrap"); err != nil {
		return MetadataResolver{}, err
	}
	if python, err = resolveExecutable(python, "python3"); err != nil {
		return MetadataResolver{}, err
	}
	return MetadataResolver{
		YTDLP: ytdlp, Node: node,
		runner: IsolatedMetadataRunner{Bubblewrap: bubblewrap, Python: python, Node: node},
	}, nil
}

var _ sandboxedRunner = IsolatedMetadataRunner{}
