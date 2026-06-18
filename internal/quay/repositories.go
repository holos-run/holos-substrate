package quay

import (
	"context"
	"net/http"
	"net/url"
)

// Repository is the subset of a Quay repository the reconciler reads back.
type Repository struct {
	// Namespace is the owning organization (or user) name.
	Namespace string `json:"namespace"`
	// Name is the repository name within the namespace.
	Name string `json:"name"`
	// Description is the repository description.
	Description string `json:"description,omitempty"`
	// IsPublic reports whether the repository is world-readable. Quay returns
	// visibility as this boolean on GET rather than the public/private string
	// used on create.
	IsPublic bool `json:"is_public"`
}

// createRepositoryRequest is the POST /api/v1/repository body. repo_kind is
// always "image" for the container repositories this controller manages.
type createRepositoryRequest struct {
	Namespace   string `json:"namespace"`
	Repository  string `json:"repository"`
	Visibility  string `json:"visibility"`
	Description string `json:"description"`
	RepoKind    string `json:"repo_kind"`
}

// CreateRepository creates an image repository named repo in namespace ns with
// the given visibility ("public" or "private") and description via
// POST /api/v1/repository (repo_kind "image").
//
// An already-existing repository is returned as an *APIError reporting
// IsConflict; use CreateRepositoryIfNotExists to treat that as success.
func (c *Client) CreateRepository(ctx context.Context, ns, repo, visibility, description string) error {
	req := createRepositoryRequest{
		Namespace:   ns,
		Repository:  repo,
		Visibility:  visibility,
		Description: description,
		RepoKind:    "image",
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/repository", req, nil)
	return mapDuplicateToConflict(err)
}

// CreateRepositoryIfNotExists creates the repository and returns nil when it
// already exists, so the call is idempotent across reconciler re-runs.
func (c *Client) CreateRepositoryIfNotExists(ctx context.Context, ns, repo, visibility, description string) error {
	err := c.CreateRepository(ctx, ns, repo, visibility, description)
	if IsConflict(err) {
		return nil
	}
	return err
}

// GetRepository fetches the repository ns/repo via
// GET /api/v1/repository/{ns}/{repo}. A missing repository is returned as an
// *APIError reporting IsNotFound.
func (c *Client) GetRepository(ctx context.Context, ns, repo string) (*Repository, error) {
	out := &Repository{}
	if err := c.doJSON(ctx, http.MethodGet, repositoryPath(ns, repo), nil, out); err != nil {
		return nil, err
	}
	// Quay's GET response omits namespace/name; populate them from the request
	// so callers always have the full identity.
	if out.Namespace == "" {
		out.Namespace = ns
	}
	if out.Name == "" {
		out.Name = repo
	}
	return out, nil
}

// changeVisibilityRequest is the POST .../changevisibility body.
type changeVisibilityRequest struct {
	Visibility string `json:"visibility"`
}

// UpdateRepositoryVisibility sets the repository's visibility ("public" or
// "private") via POST /api/v1/repository/{ns}/{repo}/changevisibility.
func (c *Client) UpdateRepositoryVisibility(ctx context.Context, ns, repo, visibility string) error {
	req := changeVisibilityRequest{Visibility: visibility}
	return c.doJSON(ctx, http.MethodPost, repositoryPath(ns, repo)+"/changevisibility", req, nil)
}

// updateDescriptionRequest is the PUT /api/v1/repository/{ns}/{repo} body.
type updateDescriptionRequest struct {
	Description string `json:"description"`
}

// UpdateRepositoryDescription sets the repository's description via
// PUT /api/v1/repository/{ns}/{repo}.
func (c *Client) UpdateRepositoryDescription(ctx context.Context, ns, repo, description string) error {
	req := updateDescriptionRequest{Description: description}
	return c.doJSON(ctx, http.MethodPut, repositoryPath(ns, repo), req, nil)
}

// DeleteRepository deletes the repository ns/repo via
// DELETE /api/v1/repository/{ns}/{repo}. A missing repository is returned as an
// *APIError reporting IsNotFound; use DeleteRepositoryIfExists to treat that as
// success.
func (c *Client) DeleteRepository(ctx context.Context, ns, repo string) error {
	return c.doJSON(ctx, http.MethodDelete, repositoryPath(ns, repo), nil, nil)
}

// DeleteRepositoryIfExists deletes the repository and returns nil when it is
// already absent, so the call is idempotent.
func (c *Client) DeleteRepositoryIfExists(ctx context.Context, ns, repo string) error {
	err := c.DeleteRepository(ctx, ns, repo)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// repositoryPath builds the /api/v1/repository/{ns}/{repo} path with each
// segment escaped.
func repositoryPath(ns, repo string) string {
	return "/api/v1/repository/" + url.PathEscape(ns) + "/" + url.PathEscape(repo)
}
