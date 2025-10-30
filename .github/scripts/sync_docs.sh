#!/usr/bin/env bash
set -euo pipefail

REPO_URL=${REPO_URL:-github.com/gofiber/docs.git}
AUTHOR_EMAIL=${AUTHOR_EMAIL:-github-actions[bot]@users.noreply.github.com}
AUTHOR_USERNAME=${AUTHOR_USERNAME:-github-actions[bot]}
VERSION_FILE=${VERSION_FILE:-contrib_versions.json}
SOURCE_DIR=${SOURCE_DIR:-v3}
DESTINATION_DIR=${DESTINATION_DIR:-}
COMMIT_URL=${COMMIT_URL:-https://github.com/gofiber/contrib}
DOCUSAURUS_COMMAND=${DOCUSAURUS_COMMAND:-npm run docusaurus -- docs:version:contrib}

TOKEN=${TOKEN:?TOKEN environment variable is required}
EVENT=${EVENT:?EVENT environment variable is required}
TAG_NAME=${TAG_NAME:-}

# Add a small logging helper and an error trap
log() {
    printf '%s %s\n' "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" "$*"
}

trap 'log "ERROR: script failed at line ${LINENO}"' ERR

log "Starting sync_docs.sh"
log "Event: ${EVENT}"
log "Source: ${SOURCE_DIR}"
log "Destination: ${DESTINATION_DIR:-(none)}"
log "Repo: ${REPO_URL}"
log "Version file: ${VERSION_FILE}"
if [[ -n "${TAG_NAME:-}" ]]; then
    log "Tag name: ${TAG_NAME}"
fi

# Configure git author
log "Configuring git author: ${AUTHOR_USERNAME} <${AUTHOR_EMAIL}>"
git config --global user.email "${AUTHOR_EMAIL}"
git config --global user.name "${AUTHOR_USERNAME}"

log "Cloning docs repository: https://${REPO_URL}"
git clone "https://${TOKEN}@${REPO_URL}" fiber-docs
log "Clone finished: fiber-docs directory created"

if [[ "${EVENT}" == "push" ]]; then
    latest_commit=$(git rev-parse --short HEAD)
    destination="${DESTINATION_DIR}"

    rsync_source="${SOURCE_DIR}/"
    # Ensure we copy into the cloned fiber-docs directory so commits/push operate on the right repo
    rsync_destination="fiber-docs/${destination}/"

    log "Preparing to sync files from '${rsync_source}' to '${rsync_destination}'"

    mkdir -p "${rsync_destination}"
    log "Running rsync (verbose) to copy markdown files..."
    rsync -av --delete --prune-empty-dirs \
        --include '*/' \
        --include '*.md' \
        --exclude '*' \
        "${rsync_source}" "${rsync_destination}"
    log "rsync completed"

elif [[ "${EVENT}" == "release" ]]; then
    if [[ -z "${TAG_NAME}" ]]; then
        echo "TAG_NAME must be provided for release events" >&2
        exit 1
    fi

    log "Handling release event for tag: ${TAG_NAME}"

    package_name="${TAG_NAME%/*}"
    major_version="${TAG_NAME#*/}"
    major_version="${major_version%%.*}"
    new_version="${package_name}_${major_version}.x.x"

    log "Computed new version identifier for docs: ${new_version}"

    pushd fiber-docs >/dev/null
    log "Running npm ci in fiber-docs"
    npm ci

    if [[ -f ${VERSION_FILE} ]]; then
        log "Removing existing entry ${new_version} from ${VERSION_FILE} (if present)"
        jq --arg version "${new_version}" 'del(.[] | select(. == $version))' "${VERSION_FILE}" > "${VERSION_FILE}.tmp"
        mv "${VERSION_FILE}.tmp" "${VERSION_FILE}"
    fi

    log "Running Docusaurus command to add version: ${DOCUSAURUS_COMMAND} ${new_version}"
    ${DOCUSAURUS_COMMAND} "${new_version}"

    if [[ -f ${VERSION_FILE} ]]; then
        log "Sorting ${VERSION_FILE}"
        jq 'sort | reverse' "${VERSION_FILE}" > "${VERSION_FILE}.tmp"
        mv "${VERSION_FILE}.tmp" "${VERSION_FILE}"
    fi
    popd >/dev/null
    log "Release handling completed"
fi

pushd fiber-docs >/dev/null
log "Checking for changes in fiber-docs"
if git status --porcelain | grep -q .; then
    log "Changes detected - staging files"
    git add .
    if [[ "${EVENT}" == "push" ]]; then
        log "Committing changes for push event"
        git commit -m "Add docs from ${COMMIT_URL}/commit/${latest_commit}"
    else
        log "Committing changes for release event"
        git commit -m "Sync docs for release ${COMMIT_URL}/releases/tag/${TAG_NAME}"
    fi

    log "Pushing changes to ${REPO_URL}"
    git push "https://${TOKEN}@${REPO_URL}"
    log "Push completed successfully"
else
    log "No documentation changes to push. Exiting."
fi
popd >/dev/null

log "sync_docs.sh finished"
