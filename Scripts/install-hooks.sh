#!/bin/bash
set -e

# Create hooks directory if it doesn't exist
mkdir -p .git/hooks

# Copy pre-commit hook
cp Scripts/git-hooks/pre-commit .git/hooks/

# Make sure it's executable
chmod +x .git/hooks/pre-commit

echo "Git hooks installed successfully." 