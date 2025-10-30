#!/usr/bin/env bash
set -euo pipefail

REPO_URL=${REPO_URL:-github.com/gofiber/docs.git}
AUTHOR_EMAIL=${AUTHOR_EMAIL:-github-actions[bot]@users.noreply.github.com}
AUTHOR_USERNAME=${AUTHOR_USERNAME:-github-actions[bot]}
VERSION_FILE=${VERSION_FILE:-contrib_versions.json}
SOURCE_DIR=${SOURCE_DIR:-v3}
TARGET_DIR=${REPO_DIR:?REPO_DIR environment variable is required}
COMMIT_URL=${COMMIT_URL:-https://github.com/gofiber/contrib}
DOCUSAURUS_COMMAND=${DOCUSAURUS_COMMAND:-npm run docusaurus -- docs:version:contrib}

TOKEN=${TOKEN:?TOKEN environment variable is required}
EVENT=${EVENT:?EVENT environment variable is required}
TAG_NAME=${TAG_NAME:-}

# Configure git author
git config --global user.email "${AUTHOR_EMAIL}"
git config --global user.name "${AUTHOR_USERNAME}"

git clone "https://${TOKEN}@${REPO_URL}" fiber-docs

if [[ "${EVENT}" == "push" ]]; then
    latest_commit=$(git rev-parse --short HEAD)
    destination="fiber-docs/docs/${TARGET_DIR}"

    mkdir -p "${destination}"
    rsync -a --delete \
        --include '*/' \
        --include '*.md' \
        --exclude '*' \
        "${SOURCE_DIR}/" "${destination}/"

elif [[ "${EVENT}" == "release" ]]; then
    if [[ -z "${TAG_NAME}" ]]; then
        echo "TAG_NAME must be provided for release events" >&2
        exit 1
    fi

    package_name="${TAG_NAME%/*}"
    major_version="${TAG_NAME#*/}"
    major_version="${major_version%%.*}"
    new_version="${package_name}_${major_version}.x.x"

    pushd fiber-docs >/dev/null
    npm ci

    if [[ -f ${VERSION_FILE} ]]; then
        jq --arg version "${new_version}" 'del(.[] | select(. == $version))' "${VERSION_FILE}" > "${VERSION_FILE}.tmp"
        mv "${VERSION_FILE}.tmp" "${VERSION_FILE}"
    fi

    ${DOCUSAURUS_COMMAND} "${new_version}"

    if [[ -f ${VERSION_FILE} ]]; then
        jq 'sort | reverse' "${VERSION_FILE}" > "${VERSION_FILE}.tmp"
        mv "${VERSION_FILE}.tmp" "${VERSION_FILE}"
    fi
    popd >/dev/null
fi

pushd fiber-docs >/dev/null
if git status --porcelain | grep -q .; then
    git add .
    if [[ "${EVENT}" == "push" ]]; then
        git commit -m "Add docs from ${COMMIT_URL}/commit/${latest_commit}"
    else
        git commit -m "Sync docs for release ${COMMIT_URL}/releases/tag/${TAG_NAME}"
    fi

    git push "https://${TOKEN}@${REPO_URL}"
else
    echo "No documentation changes to push."
fi
popd >/dev/null
