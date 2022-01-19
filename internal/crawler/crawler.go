package crawler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/deichindianer/gitlab-ci-crawler/internal/gitlab"
	"github.com/deichindianer/gitlab-ci-crawler/internal/storage"
	"github.com/rs/zerolog"
	"golang.org/x/time/rate"
)

const gitlabCIFileName = ".gitlab-ci.yml"

type Crawler struct {
	config       *Config
	gitlabClient *gitlab.Client
	storage      storage.Storage
	logger       zerolog.Logger

	projectSetMut sync.RWMutex
	projectSet    map[string]struct{}
}

// New creates a new project crawler
// The caller is responsible for closing the neo4j driver and session
// the Crawl func handles this already.
func New(cfg *Config, logger zerolog.Logger, store storage.Storage) (*Crawler, error) {
	httpClient := &rateLimitedHTTPClient{
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
		RateLimiter: rate.NewLimiter(rate.Limit(cfg.GitlabMaxRPS), cfg.GitlabMaxRPS),
	}

	gitlabClient := gitlab.NewClient(cfg.GitlabHost, cfg.GitlabToken, httpClient)

	return &Crawler{
		config:       cfg,
		gitlabClient: gitlabClient,
		storage:      store,
		logger:       logger,
		projectSet:   make(map[string]struct{}),
	}, nil
}

// Crawl iterates through every project in the given GitLab host
// and parses the CI file, and it's includes into the given Neo4j instance
func (c *Crawler) Crawl(ctx context.Context) error {
	if c.config.StorageCleanup {
		c.logger.Info().Msg("Cleanup storage...")
		err := c.storage.RemoveAll()
		if err != nil {
			return err
		}
	}

	c.logger.Info().Msg("Starting to crawl...")
	resultChan := make(chan gitlab.Project, 200)

	go func() {
		if err := c.gitlabClient.StreamAllProjects(ctx, 100, resultChan); err != nil {
			c.logger.Err(err).Msg("stopping crawler: error in project stream")
		}

	}()

	for p := range resultChan {
		_, exists := c.projectSet[p.PathWithNamespace]
		if exists {
			continue
		}

		if err := c.updateProjectInGraph(ctx, p); err != nil {
			c.logger.Err(err).
				Str("ProjectPath", p.PathWithNamespace).
				Int("ProjectID", p.ID).
				Msg("failed to parse project")
		}
	}

	c.logger.Info().Msg("stopped crawling")
	return nil
}

func (c *Crawler) updateProjectInGraph(ctx context.Context, project gitlab.Project) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		c.projectSetMut.RLock()
		_, exists := c.projectSet[project.PathWithNamespace]
		if exists {
			c.projectSetMut.RUnlock()
			return nil
		}
		c.projectSetMut.RUnlock()
		c.projectSetMut.Lock()
		c.projectSet[project.PathWithNamespace] = struct{}{}
		c.projectSetMut.Unlock()

		if err := c.storage.CreateProjectNode(project.PathWithNamespace); err != nil {
			return fmt.Errorf("failed to write project to neo4j: %w", err)
		}

		gitlabCIFile, err := c.gitlabClient.GetRawFileFromProject(ctx, project.ID, gitlabCIFileName, project.DefaultBranch)
		if err != nil {
			if errors.Is(err, gitlab.ErrRawFileNotFound) {
				return nil
			}
			return fmt.Errorf("failed to get file %s: %w", gitlabCIFileName, err)
		}

		includes, err := c.parseIncludes(gitlabCIFile)
		if err != nil {
			return fmt.Errorf("failed to parse includes for %d: %w", project.ID, err)
		}

		includes = c.enrichIncludes(includes, project, c.config.DefaultRefName)

		for _, i := range includes {
			if i.Ref == "" {
				c.logger.Warn().
					Str("Project", i.Project).
					Str("Files", strings.Join(i.Files, ",")).
					Msg("Got empty ref")
			}

			if err := c.traverseIncludes(project.PathWithNamespace, i); err != nil {
				c.logger.Err(err).
					Str("Project", i.Project).
					Msg("failed to parse include")
			}
		}
		return nil
	}
}

func (c *Crawler) traverseIncludes(parentName string, include RemoteInclude) error {
	c.projectSetMut.Lock()
	c.projectSet[include.Project] = struct{}{}
	c.projectSetMut.Unlock()

	if err := c.storage.CreateProjectNode(include.Project); err != nil {
		return fmt.Errorf("failed to write project to neo4j: %w", err)
	}

	if err := c.storage.CreateIncludeEdge(storage.IncludeEdge{
		SourceProject: parentName,
		TargetProject: include.Project,
		Ref:           include.Ref,
		Files:         include.Files,
	}); err != nil {
		return fmt.Errorf("failed to write neo4j transaction: %w", err)
	}

	for _, ci := range include.Children {
		if err := c.traverseIncludes(ci.Project, ci); err != nil {
			return fmt.Errorf("failed to write child includes for %s: %w", ci.Project, err)
		}
	}

	return nil
}
