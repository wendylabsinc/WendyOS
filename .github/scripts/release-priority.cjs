const fs = require('node:fs');
const path = require('node:path');

const ACTIVE_STATUSES = ['queued', 'in_progress', 'pending', 'waiting', 'requested'];
// Set by build.yml's run-name. Releases that skip integration tests never use
// the device runners, so build.yml titles them differently and nightly work
// does not yield to them.
const RELEASE_TITLE_PREFIX = 'Stable release · ';

async function activeStableReleases({ github, context }) {
  const results = await Promise.all(ACTIVE_STATUSES.map(status =>
    github.paginate(github.rest.actions.listWorkflowRuns, {
      ...context.repo,
      workflow_id: 'build.yml',
      event: 'workflow_dispatch',
      status,
      per_page: 100,
    })
  ));

  const releases = new Map();
  for (const run of results.flat()) {
    if (run.display_title?.startsWith(RELEASE_TITLE_PREFIX)) {
      releases.set(String(run.id), run);
    }
  }

  // A run can change status between requests and appear in two result sets.
  return [...releases.values()];
}

// Markers are per release attempt, so re-running a release only retries
// nightly work deferred during that attempt.
function deferralName(release) {
  return `nightly-deferred-for-${release.id}-${release.run_attempt}`;
}

// Devices whose update job in this run has not finished, including the one
// running the check. Before the update matrix starts, that is all of them.
async function unfinishedDevices({ github, context, devices }) {
  const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
    ...context.repo,
    run_id: context.runId,
    filter: 'latest',
    per_page: 100,
  });
  const finished = new Set(jobs.filter(job => job.status === 'completed').map(job => job.name));
  return devices.filter(device => !finished.has(device));
}

async function readMarker({ github, context, exec, artifact }) {
  const { data } = await github.rest.actions.downloadArtifact({
    ...context.repo,
    artifact_id: artifact.id,
    archive_format: 'zip',
  });
  const zip = path.join(process.env.RUNNER_TEMP, `nightly-deferral-${artifact.id}.zip`);
  fs.writeFileSync(zip, Buffer.from(data));
  const { stdout } = await exec.getExecOutput('unzip', ['-p', zip], { silent: true });
  return JSON.parse(stdout);
}

// Whether nightly work was deferred for this release attempt, and which
// devices the retry should update. Null devices means every discovered device.
async function deferredNightly({ github, context, exec, release }) {
  const name = deferralName(release);
  const artifacts = (await github.paginate(github.rest.actions.listArtifactsForRepo, {
    ...context.repo,
    name,
    per_page: 100,
  })).filter(artifact => artifact.name === name && !artifact.expired);
  if (!artifacts.length) return { deferred: false, devices: null };

  const markers = await Promise.all(artifacts.map(artifact =>
    readMarker({ github, context, exec, artifact })));
  if (markers.some(marker => marker.devices === null)) return { deferred: true, devices: null };
  return { deferred: true, devices: [...new Set(markers.flatMap(marker => marker.devices))] };
}

module.exports = {
  RELEASE_TITLE_PREFIX,
  activeStableReleases,
  deferralName,
  unfinishedDevices,
  deferredNightly,
};
