package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/leenkabha/llm_cache/internal/plugins"
	"github.com/leenkabha/llm_cache/internal/plugins/manifest"
)

// Runner ports are fixed by the platform services the runner is built from.
const (
	EmbeddingRunnerPort = 8001
	VectorRunnerPort    = 8002
	pythonBase          = "python:3.11-slim"
)

var pyIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var backendNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// RunnerPort is the port a Python plugin's service listens on.
func RunnerPort(t plugins.Type) int {
	if t == plugins.TypeEmbeddingModel {
		return EmbeddingRunnerPort
	}
	return VectorRunnerPort
}

// BuildPythonContext assembles a build context for a Python plugin from the
// platform's own service (runnerDir) plus the developer's package (srcRoot). The
// Dockerfile is generated here from a fixed template -- a Python plugin never
// supplies its own Dockerfile, and the only values interpolated are a validated
// Python identifier and the fixed port. It returns the Dockerfile path relative
// to dest.
func BuildPythonContext(runnerDir string, m *manifest.Manifest, srcRoot, dest string, lim Limits) (string, error) {
	py := m.Spec.Python
	if py == nil {
		return "", fmt.Errorf("python plugin without spec.python")
	}
	if !pyIdentRE.MatchString(py.Module) || !backendNameRE.MatchString(py.Backend) {
		return "", fmt.Errorf("invalid python module or backend name")
	}
	typ, _ := plugins.ParseType(m.Spec.Type)
	serviceDir, reqFile := "vector_store_service", "requirements.txt"
	if typ == plugins.TypeEmbeddingModel {
		serviceDir, reqFile = "embedding_service", "requirements-core.txt"
	}
	// 1. the platform service
	svcSrc := filepath.Join(runnerDir, serviceDir)
	if err := os.MkdirAll(filepath.Join(dest, "service"), 0o755); err != nil {
		return "", err
	}
	if err := copyTree(filepath.Join(svcSrc, "app"), filepath.Join(dest, "service", "app"), Limits{MaxSourceBytes: 50 << 20, MaxFiles: 1000}); err != nil {
		return "", fmt.Errorf("runner service: %w", err)
	}
	reqData, err := os.ReadFile(filepath.Join(svcSrc, reqFile))
	if err != nil {
		return "", fmt.Errorf("runner requirements: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "service", "requirements.txt"), reqData, 0o644); err != nil {
		return "", err
	}

	// 2. the developer's package: a directory, or a single module file
	pkgRoot, err := SafeJoin(srcRoot, py.Path)
	if err != nil {
		return "", err
	}
	pluginDst := filepath.Join(dest, "plugin", py.Module)
	dirCandidate := filepath.Join(pkgRoot, py.Module)
	fileCandidate := filepath.Join(pkgRoot, py.Module+".py")
	copyLine := fmt.Sprintf("COPY plugin/%s /srv/app/plugins/%s", py.Module, py.Module)
	switch {
	case isDir(dirCandidate):
		if _, err := SafeJoin(srcRoot, filepath.Join(py.Path, py.Module)); err != nil {
			return "", err
		}
		if err := copyTree(dirCandidate, pluginDst, lim); err != nil {
			return "", fmt.Errorf("plugin package: %w", err)
		}
	case isFile(fileCandidate):
		data, err := ReadRegular(srcRoot, filepath.Join(py.Path, py.Module+".py"), lim.MaxSourceBytes)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Join(dest, "plugin"), 0o755); err != nil {
			return "", err
		}
		pluginDst += ".py"
		if err := os.WriteFile(pluginDst, data, 0o644); err != nil {
			return "", err
		}
		copyLine = fmt.Sprintf("COPY plugin/%s.py /srv/app/plugins/%s.py", py.Module, py.Module)
	default:
		return "", fmt.Errorf("python module %q was not found under %q in the repository", py.Module, py.Path)
	}

	// 3. optional plugin requirements (always present, possibly empty)
	extra := []byte{}
	if py.Requirements != "" {
		if data, err := ReadRegular(srcRoot, py.Requirements, 256<<10); err == nil {
			extra = data
		}
	}
	if err := os.WriteFile(filepath.Join(dest, "plugin_requirements.txt"), extra, 0o644); err != nil {
		return "", err
	}

	port := RunnerPort(typ)
	df := strings.Join([]string{
		"FROM " + pythonBase,
		"ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1",
		"WORKDIR /srv",
		"COPY service/requirements.txt /srv/base-requirements.txt",
		"RUN pip install -r /srv/base-requirements.txt",
		"COPY plugin_requirements.txt /srv/plugin-requirements.txt",
		"RUN pip install -r /srv/plugin-requirements.txt",
		"COPY service/app /srv/app",
		copyLine,
		fmt.Sprintf("EXPOSE %d", port),
		"USER 10001:10001",
		fmt.Sprintf(`CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "%d"]`, port),
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dest, "Dockerfile"), []byte(df), 0o644); err != nil {
		return "", err
	}
	return "Dockerfile", nil
}

func isDir(p string) bool  { i, err := os.Lstat(p); return err == nil && i.IsDir() }
func isFile(p string) bool { i, err := os.Lstat(p); return err == nil && i.Mode().IsRegular() }

// RunnerEnv is the platform-owned environment that selects the developer's
// registered backend inside a Python runner.
func RunnerEnv(m *manifest.Manifest, cfgMetric string) map[string]string {
	py := m.Spec.Python
	typ, _ := plugins.ParseType(m.Spec.Type)
	env := map[string]string{}
	if py == nil {
		return env
	}
	switch typ {
	case plugins.TypeEmbeddingModel:
		env["EMBEDDING_MODEL_BACKEND"] = py.Backend
	case plugins.TypeVectorIndex:
		env["VECTOR_INDEX_BACKEND"] = py.Backend
		env["SIMILARITY_METRIC"] = "cosine"
		if cfgMetric != "" {
			env["SIMILARITY_METRIC"] = cfgMetric
		}
	case plugins.TypeSimilarityMetric:
		env["VECTOR_INDEX_BACKEND"] = "faiss"
		env["SIMILARITY_METRIC"] = py.Backend
	}
	return env
}
