package controller

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leenkabha/llm_cache/internal/plugins/secrets"
)

// Runtime is the container backend the controller drives. The local
// implementation uses the Docker CLI; a Kubernetes implementation can satisfy
// the same interface (jobs for builds, pods and services for instances).
type Runtime interface {
	Ping(ctx context.Context) error
	// PullOrInspect makes ref available locally and returns its identity.
	PullOrInspect(ctx context.Context, ref string, allowLocal bool) (ImageInfo, error)
	Build(ctx context.Context, spec BuildSpec) ([]string, error)
	InspectImage(ctx context.Context, ref string) (ImageInfo, error)
	RemoveImage(ctx context.Context, ref string)
	// ReadFileFromImage reads one file without running the image.
	ReadFileFromImage(ctx context.Context, imageRef, path string, max int64) ([]byte, error)
	Run(ctx context.Context, spec RunSpec) error
	Remove(ctx context.Context, name string)
	Logs(ctx context.Context, name string, tail int) ([]string, error)
	// Inspect returns the state of a container this controller started, or nil.
	Inspect(ctx context.Context, name string) (*ContainerInfo, error)
	// Address returns host:port at which the controller reaches a container's port.
	Address(ctx context.Context, name string, port int) (string, error)
}

// ImageInfo identifies an image immutably.
type ImageInfo struct {
	ID         string   // sha256:<image id>
	RepoDigest string   // sha256:<manifest digest> when pulled from a registry
	Size       int64    // bytes
	Tags       []string // for logging
}

// Pinned is the immutable reference recorded and later started.
func (i ImageInfo) Pinned() string {
	if i.RepoDigest != "" {
		return i.RepoDigest
	}
	return i.ID
}

type BuildSpec struct {
	ContextDir string
	Dockerfile string // path relative to ContextDir
	Tag        string
	Labels     map[string]string
}

// RunSpec is everything needed to start an isolated plugin container. There is
// no field for a command, extra mounts, capabilities, devices or privileges:
// they cannot be requested.
type RunSpec struct {
	Name     string
	Image    string // pinned reference
	Port     int
	Env      map[string]string
	Labels   map[string]string
	CPUs     float64
	Memory   int64
	PIDs     int
	Tmpfs    int64
	Network  string // per-plugin network
	Internal bool   // no route out of the network
	Publish  bool   // publish Port on 127.0.0.1 (controller runs on the host)
	// AttachSelf is the controller's own container, joined to Network so it can
	// reach the plugin by name when Publish is false.
	AttachSelf string
}

type ContainerInfo struct {
	Running bool
	Labels  map[string]string
}

// Labels the controller puts on everything it creates.
const (
	LabelManaged = "llmcache.plugin"
	LabelID      = "llmcache.plugin.id"
	LabelEnvHash = "llmcache.plugin.envhash"
	LabelDigest  = "llmcache.plugin.digest"
)

// DockerCLI implements Runtime by executing the docker binary directly (never
// through a shell). Every argument is built from validated fields.
type DockerCLI struct {
	// Self is the controller's own container when it runs in one. It joins each
	// plugin's network, so it must leave before that network can be removed;
	// otherwise the network (and one of Docker's few address pools) leaks.
	Self string
	Bin  string
	// Env is the ENTIRE environment given to docker: nothing from the
	// controller's own environment (its token, for one) is inherited.
	Env []string
}

var _ Runtime = (*DockerCLI)(nil)

// NewDockerCLI builds a runtime that talks to whichever daemon DOCKER_HOST
// selects.
func NewDockerCLI() *DockerCLI {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir()}
	for _, k := range []string{"DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_CONTEXT", "DOCKER_BUILDKIT", "BUILDKIT_HOST"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if v := os.Getenv("DOCKER_CONFIG"); v != "" {
		env = append(env, "DOCKER_CONFIG="+v)
	} else if h := os.Getenv("HOME"); h != "" {
		env = append(env, "DOCKER_CONFIG="+h+"/.docker")
	}
	return &DockerCLI{Bin: "docker", Env: env}
}

func (d *DockerCLI) exec(ctx context.Context, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Env = append([]string(nil), d.Env...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return out.Bytes(), errb.Bytes(), err
}

func (d *DockerCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	out, errb, err := d.exec(ctx, nil, args...)
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", args[0], err, tail(string(errb), 400))
	}
	return out, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}

func (d *DockerCLI) Ping(ctx context.Context) error {
	_, err := d.run(ctx, "version", "--format", "{{.Server.Version}}")
	return err
}

var imageRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,299}$`)

type inspectJSON struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	RepoTags    []string `json:"RepoTags"`
	Size        int64    `json:"Size"`
}

func (d *DockerCLI) InspectImage(ctx context.Context, ref string) (ImageInfo, error) {
	if !imageRefRE.MatchString(ref) {
		return ImageInfo{}, errors.New("invalid image reference")
	}
	out, err := d.run(ctx, "image", "inspect", "--format", "{{json .}}", ref)
	if err != nil {
		return ImageInfo{}, err
	}
	var j inspectJSON
	if err := json.Unmarshal(bytes.TrimSpace(out), &j); err != nil {
		return ImageInfo{}, fmt.Errorf("unreadable image metadata: %w", err)
	}
	info := ImageInfo{ID: j.ID, Size: j.Size, Tags: j.RepoTags}
	sort.Strings(j.RepoDigests)
	for _, rd := range j.RepoDigests {
		if i := strings.Index(rd, "@sha256:"); i >= 0 {
			info.RepoDigest = rd[i+1:]
			break
		}
	}
	return info, nil
}

func (d *DockerCLI) PullOrInspect(ctx context.Context, ref string, allowLocal bool) (ImageInfo, error) {
	if !imageRefRE.MatchString(ref) || strings.HasPrefix(ref, "-") {
		return ImageInfo{}, errors.New("invalid image reference")
	}
	if allowLocal {
		if info, err := d.InspectImage(ctx, ref); err == nil {
			return info, nil
		}
	}
	if _, err := d.run(ctx, "pull", "--quiet", ref); err != nil {
		return ImageInfo{}, err
	}
	info, err := d.InspectImage(ctx, ref)
	if err != nil {
		return ImageInfo{}, err
	}
	if info.RepoDigest == "" && !allowLocal {
		return ImageInfo{}, errors.New("the image has no registry digest to pin")
	}
	return info, nil
}

func (d *DockerCLI) Build(ctx context.Context, spec BuildSpec) ([]string, error) {
	args := []string{"build", "--tag", spec.Tag, "--file", spec.Dockerfile}
	keys := make([]string, 0, len(spec.Labels))
	for k := range spec.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	// No --build-arg, no --secret, no --ssh, no --allow: a build receives nothing
	// from the controller or from the plugin's configuration.
	args = append(args, spec.ContextDir)
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	// BuildKit is used when the daemon/client default selects it; an operator can
	// force it with DOCKER_BUILDKIT on the controller (passed through in Env).
	cmd.Env = append([]string(nil), d.Env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	lines := scrubLines(out.String(), 300)
	if err != nil {
		if ctx.Err() != nil {
			return lines, fmt.Errorf("build exceeded its time limit: %w", ctx.Err())
		}
		last := lines
		if len(last) > 8 {
			last = last[len(last)-8:]
		}
		return lines, fmt.Errorf("docker build failed: %w: %s", err, strings.Join(last, " | "))
	}
	return lines, nil
}

func (d *DockerCLI) RemoveImage(ctx context.Context, ref string) {
	if imageRefRE.MatchString(ref) {
		_, _, _ = d.exec(ctx, nil, "image", "rm", "--force", ref)
	}
}

// ReadFileFromImage extracts one file with `docker create` + `docker cp`. The
// image is never started.
func (d *DockerCLI) ReadFileFromImage(ctx context.Context, imageRef, path string, max int64) ([]byte, error) {
	if !imageRefRE.MatchString(imageRef) || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \n\x00") {
		return nil, errors.New("invalid image file request")
	}
	out, err := d.run(ctx, "create", "--pull=never", "--network=none", imageRef)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(out))
	if !regexp.MustCompile(`^[a-f0-9]{12,64}$`).MatchString(id) {
		return nil, errors.New("unexpected container id")
	}
	defer d.exec(context.Background(), nil, "rm", "--force", id)
	raw, _, err := d.exec(ctx, nil, "cp", id+":"+path, "-")
	if err != nil {
		return nil, fmt.Errorf("%s not found in the image", path)
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	hdr, err := tr.Next()
	if err != nil || hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("%s in the image is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(tr, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s in the image is larger than %d bytes", path, max)
	}
	return data, nil
}

// Run starts a hardened container. Every isolation setting is fixed here; the
// caller can only choose resource sizes, the network, and the environment.
func (d *DockerCLI) Run(ctx context.Context, s RunSpec) error {
	if s.Network != "" {
		args := []string{"network", "create", "--driver", "bridge", "--label", LabelManaged + "=true"}
		if s.Internal {
			args = append(args, "--internal")
		}
		if _, err := d.run(ctx, append(args, s.Network)...); err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	envFile, cleanup, err := writeEnvFile(s.Env)
	if err != nil {
		return err
	}
	defer cleanup()

	args := RunArgs(s, envFile)
	if _, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("start container: %s", secrets.Scrub(err.Error()))
	}
	if !s.Publish && s.AttachSelf != "" {
		if _, err := d.run(ctx, "network", "connect", s.Network, s.AttachSelf); err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}
	return nil
}

// writeEnvFile passes the environment through a 0600 file so secrets never
// appear in the docker client's argument list (visible in `ps`).
func writeEnvFile(env map[string]string) (string, func(), error) {
	f, err := os.CreateTemp("", "plugin-env-*")
	if err != nil {
		return "", func() {}, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := env[k]
		if strings.ContainsAny(k, "=\n\x00 ") || strings.ContainsAny(v, "\n\r\x00") {
			f.Close()
			os.Remove(f.Name())
			return "", func() {}, fmt.Errorf("environment variable %q cannot be passed (newlines are not allowed)", k)
		}
		fmt.Fprintf(f, "%s=%s\n", k, v)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

func (d *DockerCLI) Remove(ctx context.Context, name string) {
	if !containerNameRE.MatchString(name) {
		return
	}
	_, _, _ = d.exec(ctx, nil, "rm", "--force", "--volumes", name)
	if d.Self != "" {
		_, _, _ = d.exec(ctx, nil, "network", "disconnect", "--force", name, d.Self)
	}
	_, _, _ = d.exec(ctx, nil, "network", "rm", name)
}

var containerNameRE = regexp.MustCompile(`^llmcache-plugin-[a-z0-9_-]{1,64}$`)

func (d *DockerCLI) Logs(ctx context.Context, name string, tailN int) ([]string, error) {
	if !containerNameRE.MatchString(name) {
		return nil, errors.New("invalid container name")
	}
	out, errb, err := d.exec(ctx, nil, "logs", "--tail", strconv.Itoa(tailN), name)
	if err != nil {
		return nil, fmt.Errorf("no logs: %s", tail(string(errb), 200))
	}
	return scrubLines(string(out)+string(errb), tailN), nil
}

func (d *DockerCLI) Inspect(ctx context.Context, name string) (*ContainerInfo, error) {
	if !containerNameRE.MatchString(name) {
		return nil, errors.New("invalid container name")
	}
	out, errb, err := d.exec(ctx, nil, "container", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		if strings.Contains(string(errb), "No such") {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect: %s", tail(string(errb), 200))
	}
	var j struct {
		State  struct{ Running bool }
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &j); err != nil {
		return nil, err
	}
	return &ContainerInfo{Running: j.State.Running, Labels: j.Config.Labels}, nil
}

func (d *DockerCLI) Address(ctx context.Context, name string, port int) (string, error) {
	if !containerNameRE.MatchString(name) {
		return "", errors.New("invalid container name")
	}
	out, err := d.run(ctx, "port", name, strconv.Itoa(port)+"/tcp")
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	i := strings.LastIndex(line, ":")
	if i < 0 {
		return "", fmt.Errorf("no published port for %s", name)
	}
	return "127.0.0.1:" + line[i+1:], nil
}

// scrubLines splits output into at most max sanitised lines (the tail).
func scrubLines(s string, max int) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r", "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		l = secrets.Scrub(l)
		if len(l) > 400 {
			l = l[:400] + "..."
		}
		lines = append(lines, l)
	}
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return lines
}

// RunArgs builds the complete `docker run` argument list for a plugin. Every
// isolation setting is fixed here, so it is also what tests inspect: there is no
// way for a caller to add a privileged flag, a mount, a device or a capability.
func RunArgs(s RunSpec, envFile string) []string {
	args := []string{
		"run", "--detach", "--pull=never", "--name", s.Name,
		"--read-only",
		"--tmpfs", fmt.Sprintf("/tmp:rw,noexec,nosuid,nodev,size=%d", s.Tmpfs),
		"--user", "10001:10001",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(s.PIDs),
		"--memory", strconv.FormatInt(s.Memory, 10),
		"--memory-swap", strconv.FormatInt(s.Memory, 10),
		"--cpus", strconv.FormatFloat(s.CPUs, 'f', 3, 64),
		"--init",
		"--restart", "on-failure:5",
		"--env-file", envFile,
		"--network", s.Network,
		"--log-opt", "max-size=5m", "--log-opt", "max-file=2",
	}
	if s.Publish {
		args = append(args, "--publish", "127.0.0.1::"+strconv.Itoa(s.Port))
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	args = append(args, s.Image)
	return args
}
