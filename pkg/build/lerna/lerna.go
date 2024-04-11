package lerna

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/grafana/grafana/pkg/build/config"
	"github.com/grafana/grafana/pkg/build/fsutil"
)

// BuildFrontendPackages will bump the version for the package to the latest canary build
// and build the packages so they are ready for being published, used for generating docs etc.
func BuildFrontendPackages(version string, mode config.Edition, grafanaDir string) error {
	err := bumpLernaVersion(version, grafanaDir)
	if err != nil {
		return err
	}
	cmd := exec.Command("yarn", "run", "packages:build")
	cmd.Dir = grafanaDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to build %s frontend packages: %s", mode, output)
	}

	return nil
}

func bumpLernaVersion(version string, grafanaDir string) error {
	//nolint:gosec
	cmd := exec.Command("yarn", "nx", "release", "version", version, "--group", "grafanaPackages")
	cmd.Dir = grafanaDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to bump version for frontend packages: %s\n%s", err, output)
	}

	return nil
}

func PackFrontendPackages(ctx context.Context, tag, grafanaDir, artifactsDir string) error {
	exists, err := fsutil.Exists(artifactsDir)
	if err != nil {
		return err
	}
	if exists {
		err = os.RemoveAll(artifactsDir)
		if err != nil {
			return err
		}
	}
	// nolint:gosec
	if err = os.MkdirAll(artifactsDir, 0755); err != nil {
		return err
	}

	nxJSONPath := filepath.Join(grafanaDir, "nx.json")
	nxJson, err := os.ReadFile(nxJSONPath)
	if err != nil {
		return fmt.Errorf("failed to read nx.json: %w", err)
	}

	var nxConf NxConf

	if err := json.Unmarshal(nxJson, &nxConf); err != nil {
		return fmt.Errorf("failed to unmarshall nx.json: %w", err)
	}

	packagesToPack := nxConf.Release.Groups.GrafanaPackages.Projects
	grafanaPackages := strings.Join(packagesToPack, ",")

	// nolint:gosec
	cmd := exec.CommandContext(ctx, "yarn", "nx", "exec", fmt.Sprintf(`--projects="%s"`, grafanaPackages), "--", "yarn", "pack", "--out", fmt.Sprintf("%s/%%s-%v.tgz", artifactsDir, tag))
	cmd.Dir = grafanaDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("command '%s' failed to run, output: %s, err: %q", cmd.String(), output, err)
	}

	return nil
}

type NxConf struct {
	Release Release `json:"release"`
}
type GrafanaPackages struct {
	Projects []string `json:"projects"`
}
type Groups struct {
	GrafanaPackages GrafanaPackages `json:"grafanaPackages"`
}
type Release struct {
	ProjectsRelationship string `json:"projectsRelationship"`
	Groups               Groups `json:"groups"`
}
