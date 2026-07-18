package profile

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite"
)

type DiscoverOptions struct {
	Home string
}

type ProjectCandidate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Threads   int    `json:"threads"`
	GitRepos  int    `json:"git_repos"`
	AllClean  bool   `json:"all_clean"`
	Available bool   `json:"available"`
}

type DiscoverResult struct {
	Status   string             `json:"status"`
	Home     string             `json:"home"`
	Projects []ProjectCandidate `json:"projects"`
}

func Discover(opts DiscoverOptions) (*DiscoverResult, error) {
	home, err := resolveHome(opts.Home)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex-global-state.json"))
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	localProjects := mapValue(state["local-projects"])
	result := &DiscoverResult{Status: "ok", Home: home}
	for id, raw := range localProjects {
		project := mapValue(raw)
		name := stringValue(project["name"])
		for _, root := range stringSlice(project["rootPaths"]) {
			candidate := ProjectCandidate{ID: id, Name: name, Path: root}
			if candidate.Name == "" {
				candidate.Name = filepath.Base(root)
			}
			if info, statErr := os.Stat(root); statErr == nil && info.IsDir() {
				candidate.Available = true
				described, describeErr := describeProjects([]string{root}, filepath.Dir(root))
				if describeErr == nil && len(described) == 1 {
					candidate.GitRepos = len(described[0].Repos)
					candidate.AllClean = candidate.GitRepos > 0
					for _, repo := range described[0].Repos {
						candidate.AllClean = candidate.AllClean && repo.Clean
					}
				}
			}
			result.Projects = append(result.Projects, candidate)
		}
	}
	if err := addThreadCounts(filepath.Join(home, "state_5.sqlite"), result.Projects); err != nil {
		return nil, err
	}
	sort.Slice(result.Projects, func(i, j int) bool {
		if result.Projects[i].Name == result.Projects[j].Name {
			return result.Projects[i].Path < result.Projects[j].Path
		}
		return result.Projects[i].Name < result.Projects[j].Name
	})
	return result, nil
}

func addThreadCounts(dbPath string, projects []ProjectCandidate) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	for index := range projects {
		path := filepath.Clean(projects[index].Path)
		prefix := path + string(filepath.Separator)
		if err := db.QueryRow("select count(*) from threads where cwd = ? or substr(cwd, 1, ?) = ?", path, len(prefix), prefix).Scan(&projects[index].Threads); err != nil {
			return fmt.Errorf("count threads for %s: %w", path, err)
		}
	}
	return nil
}
