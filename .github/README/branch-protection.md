# Branch Protection Configuration

This document describes how to configure branch protection for the `main` branch to require that the "Swift Tests" status check passes before code can be merged.

## Overview

Branch protection rules cannot be configured through repository files alone—they must be set via the GitHub API or web UI. To make this process auditable and reproducible, we provide a GitHub Actions workflow that automates the configuration.

## Prerequisites

To run the branch protection workflow, you need:

1. **Repository administrator access** - Only repository admins can configure branch protection
2. **Personal Access Token (PAT)** - A token with appropriate permissions (see below)

## Setting Up the REPO_ADMIN_TOKEN Secret

The workflow requires a repository secret named `REPO_ADMIN_TOKEN` containing a GitHub Personal Access Token with the following permissions:

### For Personal Repositories
- **repo** (Full control of private repositories) scope

### For Organization Repositories
- Both **admin:org** and **repo** scopes

### Creating the Personal Access Token

1. Go to GitHub Settings → Developer settings → Personal access tokens → Tokens (classic)
2. Click "Generate new token (classic)"
3. Give it a descriptive name like "Branch Protection Configuration"
4. Select the appropriate scopes (see above)
5. Set an appropriate expiration (recommended: 90 days or less)
6. Click "Generate token" and copy the token value

### Adding the Secret to the Repository

1. Navigate to your repository on GitHub
2. Go to Settings → Secrets and variables → Actions
3. Click "New repository secret"
4. Name: `REPO_ADMIN_TOKEN`
5. Value: Paste your personal access token
6. Click "Add secret"

## Running the Workflow

Once the `REPO_ADMIN_TOKEN` secret is configured:

1. Navigate to the "Actions" tab in your repository
2. Select "Set Branch Protection" from the workflow list
3. Click "Run workflow" button
4. Select the branch (usually `main`)
5. Click the green "Run workflow" button

The workflow will execute and configure branch protection automatically.

## What the Workflow Configures

The workflow applies the following branch protection rules to the `main` branch:

### Required Status Checks
- **Required check**: `Swift Tests`
- **Strict status checks**: Enabled (branches must be up to date before merging)

### Pull Request Reviews
- **Required approving reviews**: 1
- **Dismiss stale reviews**: Enabled (new commits dismiss previous approvals)
- **Code owner reviews**: Not required

### Additional Protection
- **Force pushes**: Disabled
- **Branch deletion**: Disabled

## Verifying the Configuration

After running the workflow, you can verify the configuration:

1. Go to Settings → Branches in your repository
2. Look for the branch protection rule for `main`
3. Verify that "Swift Tests" appears in the required status checks

Alternatively, check the workflow run logs to see the API response confirming the configuration.

## Troubleshooting

### Workflow Fails with "REPO_ADMIN_TOKEN secret is not set"
- Ensure you've created the `REPO_ADMIN_TOKEN` secret as described above
- The secret name must match exactly (case-sensitive)

### Workflow Fails with HTTP 401 (Authentication Failed)
- Verify the token hasn't expired
- Ensure the token has the correct scopes/permissions
- Try generating a new token and updating the secret

### Workflow Fails with HTTP 403 (Permission Denied)
- Verify you have admin access to the repository
- For organization repositories, ensure the token has `admin:org` scope if needed
- Check that the organization allows personal access tokens

### Workflow Fails with HTTP 404 (Not Found)
- Verify the `main` branch exists in the repository
- Ensure the repository is accessible with the provided token

## Security Considerations

- **Never commit personal access tokens to the repository**
- Store tokens only in GitHub Secrets
- Use tokens with the minimum required permissions
- Set reasonable expiration dates on tokens
- Rotate tokens regularly
- The workflow runs only on manual dispatch—it will never run automatically

## Alternative: Manual Configuration via Web UI

If you prefer not to use the workflow, you can configure branch protection manually:

1. Go to Settings → Branches
2. Click "Add rule" or edit existing rule for `main`
3. Enable "Require status checks to pass before merging"
4. Enable "Require branches to be up to date before merging"
5. Search for and select "Swift Tests" in the status checks list
6. Optionally enable "Require a pull request before merging"
7. Click "Create" or "Save changes"

Note that the manual approach requires the same admin permissions and is less auditable than using the workflow.
