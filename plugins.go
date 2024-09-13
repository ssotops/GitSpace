package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"plugin"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/log"
	"github.com/pelletier/go-toml/v2"
	"github.com/ssotspace/gitspace/lib"
)

type PluginManifest struct {
	Plugin struct {
		Name        string `toml:"name"`
		Version     string `toml:"version"`
		Description string `toml:"description,omitempty"`
		Author      string `toml:"author,omitempty"`
		Sources     []struct {
			Path       string `toml:"path"`
			EntryPoint string `toml:"entry_point"`
			Repository struct {
				Type   string `toml:"type,omitempty"`
				URL    string `toml:"url,omitempty"`
				Branch string `toml:"branch,omitempty"`
			} `toml:"repository,omitempty"`
		} `toml:"sources"`
	} `toml:"plugin"`
}

type GitspacePlugin interface {
	Run() error
	Name() string
	Version() string
}

func getPluginsDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home directory: %w", err)
	}
	pluginsDir := filepath.Join(homeDir, ".ssot", "gitspace", "plugins")

	// Ensure the plugins directory exists
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create plugins directory: %w", err)
	}

	return pluginsDir, nil
}

func installPlugin(logger *log.Logger, source string) error {
	logger.Debug("Starting plugin installation", "source", source)

	pluginsDir, err := getPluginsDir()
	if err != nil {
		return fmt.Errorf("failed to get plugins directory: %w", err)
	}
	logger.Debug("Plugins directory", "path", pluginsDir)

	isRemote := strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
	isCatalog := strings.HasPrefix(source, "catalog://")
	logger.Debug("Source type", "isRemote", isRemote, "isCatalog", isCatalog)

	var manifestPath string
	var sourceDir string

	if isCatalog {
		// Handle Gitspace Catalog installation
		catalogItem := strings.TrimPrefix(source, "catalog://")
		return installFromGitspaceCatalog(logger, catalogItem)
	} else if isRemote {
		// Handle remote installation
		tempDir, err := os.MkdirTemp("", "gitspace-plugin-*")
		if err != nil {
			return fmt.Errorf("failed to create temporary directory: %w", err)
		}
		defer os.RemoveAll(tempDir)

		logger.Debug("Cloning remote repository", "source", source, "tempDir", tempDir)
		err = gitClone(source, tempDir)
		if err != nil {
			return fmt.Errorf("failed to clone remote repository: %w", err)
		}

		manifestPath = filepath.Join(tempDir, "gitspace-plugin.toml")
		sourceDir = tempDir
	} else {
		// Handle local installation
		absSource, err := filepath.Abs(source)
		if err != nil {
			return fmt.Errorf("failed to get absolute path of source: %w", err)
		}
		logger.Debug("Absolute source path", "path", absSource)

		sourceInfo, err := os.Stat(absSource)
		if err != nil {
			return fmt.Errorf("failed to get source info: %w", err)
		}

		if sourceInfo.IsDir() {
			manifestPath = filepath.Join(absSource, "gitspace-plugin.toml")
			sourceDir = absSource
		} else if filepath.Ext(absSource) == ".toml" {
			manifestPath = absSource
			sourceDir = filepath.Dir(absSource)
		} else {
			return fmt.Errorf("invalid source: must be a directory or .toml file")
		}
	}

	logger.Debug("Loading plugin manifest", "path", manifestPath)
	manifest, err := loadPluginManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to load manifest: %w", err)
	}
	logger.Debug("Loaded plugin manifest", "name", manifest.Plugin.Name)

	// Create a directory for the plugin in the plugins directory
	pluginDir := filepath.Join(pluginsDir, manifest.Plugin.Name)
	logger.Debug("Preparing plugin directory", "path", pluginDir)

	// Remove existing plugin directory if it exists
	if err := os.RemoveAll(pluginDir); err != nil {
		return fmt.Errorf("failed to remove existing plugin directory: %w", err)
	}

	// Create the plugin directory
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		return fmt.Errorf("failed to create plugin directory: %w", err)
	}

	// Copy the manifest file
	destManifestPath := filepath.Join(pluginDir, "gitspace-plugin.toml")
	logger.Debug("Copying manifest file", "from", manifestPath, "to", destManifestPath)
	if err := copyFile(manifestPath, destManifestPath); err != nil {
		return fmt.Errorf("failed to copy manifest file: %w", err)
	}

	// Copy the plugin source files
	logger.Debug("Copying plugin source files", "sourceDir", sourceDir)
	for _, source := range manifest.Plugin.Sources {
		sourcePath := filepath.Join(sourceDir, source.Path)
		destPath := filepath.Join(pluginDir, source.Path)

		logger.Debug("Copying file", "from", sourcePath, "to", destPath)
		if err := copyFile(sourcePath, destPath); err != nil {
			return fmt.Errorf("failed to copy plugin file %s: %w", source.Path, err)
		}
	}

	// Create or update go.mod file
	goModPath := filepath.Join(pluginDir, "go.mod")
	if _, err := os.Stat(goModPath); os.IsNotExist(err) {
		logger.Debug("Creating go.mod file", "path", goModPath)
		cmd := exec.Command("go", "mod", "init", fmt.Sprintf("github.com/ssotops/gitspace/plugins/%s", manifest.Plugin.Name))
		cmd.Dir = pluginDir
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to initialize go.mod: %w\nOutput: %s", err, output)
		}
	}

	// Update go.mod to use the same Go version as the main program
	logger.Debug("Updating go.mod version", "path", goModPath)
	goVersion := strings.TrimPrefix(runtime.Version(), "go")
	cmd := exec.Command("go", "mod", "edit", "-go", goVersion)
	cmd.Dir = pluginDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to update go.mod version: %w\nOutput: %s", err, output)
	}

	// Run go mod tidy to ensure all dependencies are properly managed
	logger.Debug("Running go mod tidy", "path", pluginDir)
	cmd = exec.Command("go", "mod", "tidy")
	cmd.Dir = pluginDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to tidy go.mod: %w\nOutput: %s", err, output)
	}

	logger.Info("Plugin installed successfully", "name", manifest.Plugin.Name, "path", pluginDir)
	return nil
}

func installFromGitspaceCatalog(logger *log.Logger, catalogItem string) error {
	owner := "ssotops"
	repo := "gitspace-catalog"
	defaultBranch := "master"
	catalog, err := fetchGitspaceCatalog(owner, repo)
	if err != nil {
		return fmt.Errorf("failed to fetch Gitspace Catalog: %w", err)
	}

	plugin, ok := catalog.Plugins[catalogItem]
	if !ok {
		return fmt.Errorf("plugin %s not found in Gitspace Catalog", catalogItem)
	}

	if plugin.Path == "" {
		return fmt.Errorf("no path found for plugin %s in Gitspace Catalog", catalogItem)
	}

	// Construct the raw GitHub URL for the plugin directory
	rawGitHubURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, defaultBranch, plugin.Path)

	// Create a temporary directory for the plugin
	tempDir, err := os.MkdirTemp("", "gitspace-plugin-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Download the gitspace-plugin.toml file
	manifestURL := fmt.Sprintf("%s/gitspace-plugin.toml", rawGitHubURL)
	manifestPath := filepath.Join(tempDir, "gitspace-plugin.toml")
	err = downloadFile(manifestURL, manifestPath)
	if err != nil {
		return fmt.Errorf("failed to download gitspace-plugin.toml: %w", err)
	}

	// Parse the gitspace-plugin.toml file
	pluginManifest, err := loadPluginManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to load plugin manifest: %w", err)
	}

	// Download each source file specified in the manifest
	for _, source := range pluginManifest.Plugin.Sources {
		sourceURL := fmt.Sprintf("%s/%s", rawGitHubURL, source.Path)
		sourcePath := filepath.Join(tempDir, source.Path)
		err = downloadFile(sourceURL, sourcePath)
		if err != nil {
			return fmt.Errorf("failed to download source file %s: %w", source.Path, err)
		}
	}

	// Now install the plugin from the temporary directory
	return installPlugin(logger, tempDir)
}

func downloadFile(url string, filepath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func loadPluginManifest(path string) (*PluginManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest file: %w", err)
	}

	var manifest PluginManifest
	err = toml.Unmarshal(data, &manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to decode manifest: %w", err)
	}
	return &manifest, nil
}

func copyFile(src, dst string) error {
	// Ensure the destination directory exists
	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	input, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("failed to read source file: %w", err)
	}

	err = os.WriteFile(dst, input, 0644)
	if err != nil {
		return fmt.Errorf("failed to write destination file: %w", err)
	}

	return nil
}

func uninstallPlugin(logger *log.Logger, name string) error {
	pluginsDir, err := getPluginsDir()
	if err != nil {
		return fmt.Errorf("failed to get plugins directory: %w", err)
	}

	pluginDir := filepath.Join(pluginsDir, name)
	if err := os.RemoveAll(pluginDir); err != nil {
		return fmt.Errorf("failed to remove plugin directory: %w", err)
	}

	logger.Info("Plugin uninstalled successfully", "name", name)
	return nil
}

func printInstalledPlugins(logger *log.Logger) error {
	pluginsDir, err := getPluginsDir()
	if err != nil {
		return fmt.Errorf("failed to get plugins directory: %w", err)
	}

	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return fmt.Errorf("failed to read plugins directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			fmt.Println(entry.Name())
		}
	}

	return nil
}

func runPlugin(logger *log.Logger) error {
	pluginsDir, err := getPluginsDir()
	if err != nil {
		return fmt.Errorf("failed to get plugins directory: %w", err)
	}

	plugins, err := listInstalledPlugins(pluginsDir)
	if err != nil {
		return fmt.Errorf("failed to list installed plugins: %w", err)
	}

	if len(plugins) == 0 {
		logger.Info("No plugins installed")
		return nil
	}

	var selectedPlugin string
	err = huh.NewSelect[string]().
		Title("Select a plugin to run").
		Options(plugins...).
		Value(&selectedPlugin).
		Run()

	if err != nil {
		return fmt.Errorf("failed to select plugin: %w", err)
	}

	pluginDir := filepath.Join(pluginsDir, selectedPlugin)
	return compileAndRunPlugin(logger, pluginDir, selectedPlugin)
}

func listInstalledPlugins(pluginsDir string) ([]huh.Option[string], error) {
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read plugins directory: %w", err)
	}

	var options []huh.Option[string]
	for _, entry := range entries {
		if entry.IsDir() {
			options = append(options, huh.NewOption(entry.Name(), entry.Name()))
		}
	}
	return options, nil
}

func compileAndRunPlugin(logger *log.Logger, pluginDir, pluginName string) error {
	logger.Debug("Starting compileAndRunPlugin", "pluginDir", pluginDir, "pluginName", pluginName)

	// Find the main Go file
	mainFile := ""
	err := filepath.Walk(pluginDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") {
			mainFile = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		logger.Error("Failed to find plugin source file", "error", err)
		return fmt.Errorf("failed to find plugin source file: %w", err)
	}
	if mainFile == "" {
		logger.Error("No Go source file found in plugin directory")
		return fmt.Errorf("no Go source file found in plugin directory")
	}

	logger.Debug("Found main plugin file", "path", mainFile)

	// Compile the plugin
	pluginFile := filepath.Join(pluginDir, pluginName+".so")
	buildCmd := exec.Command("go", "build", "-buildmode=plugin", "-o", pluginFile, mainFile)
	buildCmd.Dir = pluginDir
	buildCmd.Env = append(os.Environ(),
		"CGO_ENABLED=1",
		fmt.Sprintf("GOARCH=%s", runtime.GOARCH),
		fmt.Sprintf("GOOS=%s", runtime.GOOS),
	)

	output, err := buildCmd.CombinedOutput()
	if err != nil {
		logger.Error("Plugin compilation failed", "output", string(output), "error", err)
		return fmt.Errorf("failed to compile plugin: %w\nOutput: %s", err, output)
	}

	logger.Debug("Plugin compiled successfully", "output", string(output))

	// Load and run the plugin
	logger.Debug("Attempting to open plugin", "path", pluginFile)
	p, err := plugin.Open(pluginFile)
	if err != nil {
		logger.Error("Failed to open plugin", "error", err)
		return fmt.Errorf("failed to open plugin: %w", err)
	}

	logger.Debug("Plugin opened successfully")

	logger.Debug("Looking up Plugin symbol")
	symPlugin, err := p.Lookup("Plugin")
	if err != nil {
		logger.Error("Failed to lookup Plugin symbol", "error", err)
		return fmt.Errorf("plugin does not have a Plugin symbol: %w", err)
	}

	logger.Debug("Found Plugin symbol")

	plugin, ok := symPlugin.(GitspacePlugin)
	if !ok {
		logger.Error("Plugin does not implement GitspacePlugin interface")
		return fmt.Errorf("plugin does not implement GitspacePlugin interface")
	}

	logger.Info("Running plugin", "name", pluginName)
	return plugin.Run()
}

func ensureGoMod(pluginDir, pluginName string) error {
	goModPath := filepath.Join(pluginDir, "go.mod")
	if _, err := os.Stat(goModPath); os.IsNotExist(err) {
		cmd := exec.Command("go", "mod", "init", fmt.Sprintf("gitspace.com/plugin/%s", pluginName))
		cmd.Dir = pluginDir
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to initialize go.mod: %w\nOutput: %s", err, output)
		}
	}

	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = pluginDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to tidy go.mod: %w\nOutput: %s", err, output)
	}

	return nil
}

func executePlugin(logger *log.Logger, pluginPath string) error {
	p, err := plugin.Open(pluginPath)
	if err != nil {
		return fmt.Errorf("failed to open plugin: %w", err)
	}

	runSymbol, err := p.Lookup("Run")
	if err != nil {
		return fmt.Errorf("plugin does not have a Run function: %w", err)
	}

	runFunc, ok := runSymbol.(func() error)
	if !ok {
		return fmt.Errorf("plugin Run function has wrong signature")
	}

	logger.Info("Running plugin", "path", pluginPath)
	return runFunc()
}

func gitClone(url, destPath string) error {
	cmd := exec.Command("git", "clone", url, destPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w\nOutput: %s", err, output)
	}
	return nil
}

func fetchGitspaceCatalog(owner, repo string) (*lib.GitspaceCatalog, error) {
	return lib.FetchGitspaceCatalog(owner, repo)
}
