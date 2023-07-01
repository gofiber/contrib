#!/usr/bin/env bash

# Some env variables
BRANCH="main"
REPO="contrib"
REPO_URL="github.com/gofiber/docs.git"
AUTHOR_EMAIL="github-actions[bot]@users.noreply.github.com"
AUTHOR_USERNAME="github-actions[bot]"

# Set commit author
git config --global user.email "${AUTHOR_EMAIL}"
git config --global user.name "${AUTHOR_USERNAME}"

# Exit if event is not PUSH
if [ $EVENT != "push" ]; then
  exit 0
fi

latest_commit=$(git rev-parse --short HEAD)

git clone https://${TOKEN}@${REPO_URL} fiber-docs
mkdir -p fiber-docs/$REPO

for f in */; do
  if [ "$f" == "fiber-docs/" ]; then
    continue
  fi

  log_output=$(git log --oneline "${BRANCH}" HEAD~1..HEAD --name-status -- "${f}README.md")
    if [[ $log_output != "" || ! -f "fiber-docs/$REPO/${f::-1}.md" ]]; then
      cp "${f}README.md" fiber-docs/$REPO/${f::-1}.md
    fi
done

# Push changes for contrib instance docs
cd fiber-docs/ || return
git add .
git commit -m "Add docs from https://github.com/gofiber/contrib/commit/${latest_commit}"
git push https://${TOKEN}@${REPO_URL}