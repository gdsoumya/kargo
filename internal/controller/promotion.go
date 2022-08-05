package controller

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	api "github.com/akuityio/k8sta/api/v1alpha1"
	argocd "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

func (t *ticketReconciler) promoteImage(
	ctx context.Context,
	ticket *api.Ticket,
	app *argocd.Application,
) (string, error) {
	// This is a critical section of code because authentication methods use by
	// the git CLI all involve touching files in the user's home directory.
	t.promoMutex.Lock()
	defer t.promoMutex.Unlock()
	defer t.tearDownGitAuthFn()
	if err := t.setupGitAuthFn(ctx, app.Spec.Source.RepoURL); err != nil {
		return "", errors.Wrapf(
			err,
			"error setting up authentication for repo %q",
			app.Spec.Source.RepoURL,
		)
	}

	// Create a temporary workspace
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		return "", errors.Wrapf(
			err,
			"error creating temporary workspace for cloning repo %q",
			app.Spec.Source.RepoURL,
		)
	}
	defer os.RemoveAll(tempDir)
	t.logger.WithFields(log.Fields{
		"path": tempDir,
	}).Debug("created temporary workspace")

	// Clone the repo
	repoDir := filepath.Join(tempDir, "repo")
	cmd := exec.Command( // nolint: gosec
		"git",
		"clone",
		"--no-tags",
		app.Spec.Source.RepoURL,
		repoDir,
	)
	if _, err = t.execCommand(cmd); err != nil {
		return "", errors.Wrapf(
			err,
			"error cloning repo %q into %q",
			app.Spec.Source.RepoURL,
			repoDir,
		)
	}
	t.logger.WithFields(log.Fields{
		"path": repoDir,
		"repo": app.Spec.Source.RepoURL,
	}).Debug("clone git repository")

	// TODO: This is hard-coded for now, but there's a possibility here of later
	// supporting other tools and patterns.
	return t.promotionStrategyRenderedYAMLBranchesWithKustomize(
		ctx,
		ticket,
		app,
		repoDir,
	)
}

// setupGitAuth, if necessary, configures the git CLI for authentication using
// either SSH or the "store" (username/password-based) credential helper.
func (t *ticketReconciler) setupGitAuth(
	ctx context.Context,
	repoURL string,
) error {
	const repoTypeGit = "git"
	var sshKey, username, password string
	// NB: This next call returns an empty Repository if no such Repository is
	// found, so instead of continuing to look for credentials if no Repository is
	// found, what we'll do is continue looking for credentials if the Repository
	// we get back doesn't have anything we can use, i.e. no SSH private key or
	// password.
	repo, err := t.argoDB.GetRepository(ctx, repoURL)
	if err != nil {
		return errors.Wrapf(
			err,
			"error getting Repository (Secret) for repo %q",
			repoURL,
		)
	}
	if repo.Type == repoTypeGit || repo.Type == "" {
		sshKey = repo.SSHPrivateKey
		username = repo.Username
		password = repo.Password
	}
	if sshKey == "" && password == "" {
		// We didn't find any creds yet, so keep looking
		var repoCreds *argocd.RepoCreds
		repoCreds, err = t.argoDB.GetRepositoryCredentials(ctx, repoURL)
		if err != nil {
			return errors.Wrapf(
				err,
				"error getting Repository Credentials (Secret) for repo %q",
				repoURL,
			)
		}
		if repoCreds.Type == repoTypeGit || repoCreds.Type == "" {
			sshKey = repo.SSHPrivateKey
			username = repo.Username
			password = repo.Password
		}
	}

	// We didn't find any creds, so we're done. We can't promote without creds.
	if sshKey == "" && password == "" {
		return errors.Errorf("could not find any credentials for repo %q", repoURL)
	}

	homeDir, err := homedir.Dir()
	if err != nil {
		return errors.Wrap(err, "error finding user's home directory")
	}

	// If an SSH key was provided, use that.
	if sshKey != "" {
		rsaKeyPath := filepath.Join(homeDir, ".ssh", "id_rsa")
		if err = ioutil.WriteFile(rsaKeyPath, []byte(sshKey), 0600); err != nil {
			return errors.Wrapf(err, "error writing SSH key to %q", rsaKeyPath)
		}
		return nil // We're done
	}

	// If we get to here, we're authenticating using a password

	credentialURL, err := url.Parse(repoURL)
	if err != nil {
		return errors.Wrapf(err, "error parsing URL %q", repoURL)
	}
	// Remove path and query string components from the URL
	credentialURL.Path = ""
	credentialURL.RawQuery = ""
	// If the username is the empty string, we assume we're working with a git
	// provider like GitHub that only requires the username to be non-empty. We
	// arbitrarily set it to "git".
	if username == "" {
		username = "git"
	}
	// Augment the URL with user/pass information.
	credentialURL.User = url.UserPassword(username, password)
	// Write the augmented URL to the location used by the "stored" credential
	// helper.
	credentialsPath := filepath.Join(homeDir, ".git-credentials")
	if err := ioutil.WriteFile(
		credentialsPath,
		[]byte(credentialURL.String()),
		0600,
	); err != nil {
		return errors.Wrapf(
			err,
			"error writing credentials to %q",
			credentialsPath,
		)
	}
	return nil
}

func (t *ticketReconciler) tearDownGitAuth() {
	homeDir, err := homedir.Dir()
	if err != nil {
		t.logger.Errorf("error finding user's home directory: %s", err)
		return
	}
	rsaKeyPath := filepath.Join(homeDir, ".ssh", "id_rsa")
	if err = os.RemoveAll(rsaKeyPath); err != nil {
		t.logger.Errorf("error deleting file %q: %s", rsaKeyPath, err)
	}
	credentialsPath := filepath.Join(homeDir, ".git-credentials")
	if err = os.RemoveAll(credentialsPath); err != nil {
		t.logger.Errorf("error deleting file %q: %s", credentialsPath, err)
	}
}

func (t *ticketReconciler) promotionStrategyRenderedYAMLBranchesWithKustomize(
	ctx context.Context,
	ticket *api.Ticket,
	app *argocd.Application,
	repoDir string,
) (string, error) {
	loggerFields := log.Fields{
		"repo":      app.Spec.Source.RepoURL,
		"envBranch": app.Spec.Source.TargetRevision,
		"imageRepo": ticket.Change.ImageRepo,
		"imageTag":  ticket.Change.ImageTag,
	}

	// We assume the environment-specific overlay path within the source branch ==
	// the name of the environment-specific branch that the final rendered YAML
	// will live in.
	envDir := filepath.Join(repoDir, app.Spec.Source.TargetRevision)

	// Set the image
	cmd := exec.Command( // nolint: gosec
		"kustomize",
		"edit",
		"set",
		"image",
		fmt.Sprintf(
			"%s=%s:%s",
			ticket.Change.ImageRepo,
			ticket.Change.ImageRepo,
			ticket.Change.ImageTag,
		),
	)
	cmd.Dir = envDir // We need to be in the overlay directory to do this
	if _, err := t.execCommand(cmd); err != nil {
		return "", errors.Wrap(err, "error setting image")
	}
	t.logger.WithFields(loggerFields).Debug("ran kustomize edit set image")

	// Render environment-specific YAML
	// TODO: We may need to buffer this or use a file instead because the rendered
	// YAML could be quite large.
	cmd = exec.Command("kustomize", "build")
	cmd.Dir = envDir // We need to be in the overlay directory to do this
	yamlBytes, err := t.execCommandFn(cmd)
	if err != nil {
		return "",
			errors.Wrapf(
				err,
				"error rendering YAML for branch %q",
				app.Spec.Source.TargetRevision,
			)
	}
	t.logger.WithFields(loggerFields).Debug("rendered environment-specific YAML")

	// Commit the changes to the source branch
	cmd = exec.Command( // nolint: gosec
		"git",
		"commit",
		"-am",
		fmt.Sprintf(
			"k8sta: updating %s to use image %s:%s",
			app.Spec.Source.TargetRevision,
			ticket.Change.ImageRepo,
			ticket.Change.ImageTag,
		),
	)
	cmd.Dir = repoDir // We need to be in the root of the repo for this
	if _, err = t.execCommand(cmd); err != nil {
		return "", errors.Wrap(err, "error committing changes to source branch")
	}
	t.logger.WithFields(loggerFields).Debug(
		"committed changes to the source branch",
	)

	// Push the changes to the source branch
	cmd = exec.Command("git", "push", "origin", "HEAD")
	cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
	if _, err = t.execCommand(cmd); err != nil {
		return "", errors.Wrap(err, "error pushing changes to source branch")
	}
	t.logger.WithFields(loggerFields).Debug("pushed changes to the source branch")

	// Switch to the env-specific branch
	// TODO: Should we do something about the possibility that the branch doesn't
	// already exist, e.g. `git checkout --orphan <envBranch> --`
	cmd = exec.Command( // nolint: gosec
		"git",
		"checkout",
		app.Spec.Source.TargetRevision,
		// The next line makes it crystal clear to git that we're checking out
		// a branch. We need to do this since we operate under an assumption that
		// the path to the overlay within the repo == the branch name.
		"--",
	)
	cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
	if _, err = t.execCommand(cmd); err != nil {
		return "", errors.Wrapf(
			err,
			"error checking out environment-specific branch %q from repo %q",
			app.Spec.Source.TargetRevision,
			app.Spec.Source.RepoURL,
		)
	}
	t.logger.WithFields(loggerFields).Debug(
		"checked out environment-specific branch",
	)

	// Remove existing rendered YAML
	files, err := filepath.Glob(filepath.Join(repoDir, "*"))
	if err != nil {
		return "", errors.Wrapf(
			err,
			"error listing files in environment-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	for _, file := range files {
		if _, fileName := filepath.Split(file); fileName == ".git" {
			continue
		}
		if err = os.RemoveAll(file); err != nil {
			return "", errors.Wrapf(
				err,
				"error deleting file %q from environment-specific branch %q",
				file,
				app.Spec.Source.TargetRevision,
			)
		}
	}
	t.logger.WithFields(loggerFields).Debug("removed existing rendered YAML")

	// Write the new rendered YAML
	if err = os.WriteFile( // nolint: gosec
		filepath.Join(repoDir, "all.yaml"),
		yamlBytes,
		0644,
	); err != nil {
		return "", errors.Wrapf(
			err,
			"error writing rendered YAML to environment-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	t.logger.WithFields(loggerFields).Debug("wrote new rendered YAML")

	// Commit the changes to the environment-specific branch
	cmd = exec.Command( // nolint: gosec
		"git",
		"commit",
		"-am",
		fmt.Sprintf(
			"k8sta: use image %s:%s",
			ticket.Change.ImageRepo,
			ticket.Change.ImageTag,
		),
	)
	cmd.Dir = repoDir // We need to be in the root of the repo for this
	if _, err = t.execCommand(cmd); err != nil {
		return "", errors.Wrapf(
			err,
			"error committing changes to environment-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	t.logger.WithFields(loggerFields).Debug(
		"committed changes to environment-specific branch",
	)

	// Push the changes to the environment-specific branch
	cmd = exec.Command( // nolint: gosec
		"git",
		"push",
		"origin",
		app.Spec.Source.TargetRevision,
	)
	cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
	if _, err = t.execCommand(cmd); err != nil {
		return "", errors.Wrapf(
			err,
			"error pushing changes to environment-specific branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	t.logger.WithFields(loggerFields).Debug(
		"pushed changes to environment-specific branch",
	)

	// Get the ID of the last commit
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoDir // We need to be anywhere in the root of the repo for this
	shaBytes, err := t.execCommandFn(cmd)
	if err != nil {
		return "", errors.Wrapf(
			err,
			"error obtaining last commit ID for branch %q",
			app.Spec.Source.TargetRevision,
		)
	}
	sha := strings.TrimSpace(string(shaBytes))
	t.logger.WithFields(loggerFields).Debug(
		"obtained sha of commit to environment-specific branch",
	)
	return sha, nil
}

func (t *ticketReconciler) execCommand(cmd *exec.Cmd) ([]byte, error) {
	return cmd.CombinedOutput()
}