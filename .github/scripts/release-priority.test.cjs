const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const {
  RELEASE_TITLE_PREFIX,
  activeStableReleases,
  deferredNightly,
  unfinishedDevices,
} = require('./release-priority.cjs');

const context = {
  repo: { owner: 'wendylabsinc', repo: 'WendyOS' },
  runId: 42,
};

process.env.RUNNER_TEMP = fs.mkdtempSync(path.join(os.tmpdir(), 'release-priority-'));
test.after(() => fs.rmSync(process.env.RUNNER_TEMP, { recursive: true, force: true }));

function release(id, changes = {}) {
  return {
    id,
    event: 'workflow_dispatch',
    status: 'queued',
    display_title: 'Stable release · main',
    ...changes,
  };
}

function mockGitHub(pagesByStatus) {
  const requests = [];
  const listWorkflowRuns = async params => {
    requests.push(params);
    return { data: {
      workflow_runs: pagesByStatus[params.status]?.[params.page - 1] ?? [],
    } };
  };
  return {
    requests,
    rest: { actions: { listWorkflowRuns } },
    async paginate(route, params) {
      assert.equal(route, listWorkflowRuns);
      assert.deepEqual(params, {
        ...context.repo,
        workflow_id: 'build.yml',
        event: 'workflow_dispatch',
        status: params.status,
        per_page: 100,
      });
      const results = [];
      let page = 1;
      while (true) {
        const response = await route({ ...params, page });
        const runs = response.data.workflow_runs;
        results.push(...runs);
        if (runs.length < params.per_page) return results;
        page += 1;
      }
    },
  };
}

test('checks every active status and keeps only stable releases that use the device runners', async () => {
  const statuses = ['queued', 'in_progress', 'pending', 'waiting', 'requested'];
  const github = mockGitHub(Object.fromEntries(statuses.map((status, index) => [
    status,
    [[release(index + 1, { status })]],
  ])));
  const original = github.paginate;
  github.paginate = async (route, params) => [
    ...await original(route, params),
    release(51, { display_title: 'Build · main' }),
    release(52, { display_title: 'Stable release without integration tests · main' }),
    release(53, { display_title: undefined }),
    release(54, { display_title: 'Stable release candidate' }),
    release(55, { display_title: 'Build products and release' }),
  ];

  const runs = await activeStableReleases({ github, context });

  assert.deepEqual(runs.map(run => run.id), [1, 2, 3, 4, 5]);
  assert.deepEqual(github.requests.map(request => request.status), statuses);
});

test('finds a release beyond the first page', async () => {
  const github = mockGitHub({
    queued: [
      Array.from({ length: 100 }, (_, index) => release(index + 100, {
        display_title: 'Build · main',
      })),
      [release(200)],
    ],
  });

  const runs = await activeStableReleases({ github, context });

  assert.deepEqual(runs.map(run => run.id), [200]);
  assert.deepEqual(github.requests.filter(request => request.status === 'queued')
    .map(request => request.page), [1, 2]);
});

test('deduplicates releases observed during a status change', async () => {
  const github = mockGitHub({
    queued: [[release(1)]],
    in_progress: [[release('1', { status: 'in_progress' })]],
  });

  const runs = await activeStableReleases({ github, context });

  assert.equal(runs.length, 1);
  assert.equal(runs[0].status, 'in_progress');
});

test('returns an empty list when no active release exists', async () => {
  const github = mockGitHub({});
  assert.deepEqual(await activeStableReleases({ github, context }), []);
});

test('propagates API failures so callers cannot admit background work', async () => {
  const github = mockGitHub({});
  const failure = new Error('GitHub API unavailable');
  github.paginate = async (_route, params) => {
    if (params.status === 'waiting') throw failure;
    return [];
  };

  await assert.rejects(activeStableReleases({ github, context }), failure);
});

test('the release title matches build.yml and the nightly retry trigger', () => {
  const workflows = path.join(__dirname, '..', 'workflows');
  const build = fs.readFileSync(path.join(workflows, 'build.yml'), 'utf8');
  const nightly = fs.readFileSync(path.join(workflows, 'nightly-os-update.yml'), 'utf8');

  assert.ok(build.includes(`format('${RELEASE_TITLE_PREFIX}{0}'`));
  // The run-level concurrency group and the retry check job.
  assert.equal(nightly.split(
    `startsWith(github.event.workflow_run.display_title, '${RELEASE_TITLE_PREFIX}')`,
  ).length - 1, 2);
});

function mockJobs(jobs) {
  const listJobsForWorkflowRun = async () => {};
  return {
    rest: { actions: { listJobsForWorkflowRun } },
    async paginate(route, params) {
      assert.equal(route, listJobsForWorkflowRun);
      assert.deepEqual(params, {
        ...context.repo,
        run_id: context.runId,
        filter: 'latest',
        per_page: 100,
      });
      return jobs;
    },
  };
}

test('defers the devices whose update job has not finished', async () => {
  const github = mockJobs([
    { name: 'Check for stable releases', status: 'completed' },
    { name: 'Discover devices', status: 'completed' },
    { name: 'a.local', status: 'completed' },
    { name: 'b.local', status: 'in_progress' },
    { name: 'c.local', status: 'queued' },
  ]);

  const devices = await unfinishedDevices({
    github, context, devices: ['a.local', 'b.local', 'c.local', 'd.local'],
  });

  assert.deepEqual(devices, ['b.local', 'c.local', 'd.local']);
});

const releaseAttempt = { id: 123, run_attempt: 2 };
const markerName = 'nightly-deferred-for-123-2';

function mockArtifacts(pages, markers = {}) {
  const requests = [];
  const listArtifactsForRepo = async params => {
    requests.push(params);
    return { data: { artifacts: pages[params.page - 1] ?? [] } };
  };
  const downloadArtifact = async params => {
    assert.deepEqual(params, {
      ...context.repo, artifact_id: params.artifact_id, archive_format: 'zip',
    });
    return { data: new ArrayBuffer(0) };
  };
  const exec = {
    async getExecOutput(command, args) {
      assert.equal(command, 'unzip');
      const id = Number(path.basename(args[1]).match(/^nightly-deferral-(\d+)\.zip$/)[1]);
      return { stdout: JSON.stringify(markers[id]) };
    },
  };
  return {
    exec,
    requests,
    rest: { actions: { listArtifactsForRepo, downloadArtifact } },
    async paginate(route, params) {
      assert.equal(route, listArtifactsForRepo);
      assert.deepEqual(params, {
        ...context.repo,
        name: markerName,
        per_page: 100,
      });
      const results = [];
      let page = 1;
      while (true) {
        const response = await route({ ...params, page });
        const artifacts = response.data.artifacts;
        results.push(...artifacts);
        if (artifacts.length < params.per_page) return results;
        page += 1;
      }
    },
  };
}

test('retries the devices deferred during this release attempt', async () => {
  const github = mockArtifacts([[
    { id: 7, name: markerName, expired: false },
  ]], { 7: { devices: ['b.local', 'c.local'] } });

  assert.deepEqual(
    await deferredNightly({ github, context, exec: github.exec, release: releaseAttempt }),
    { deferred: true, devices: ['b.local', 'c.local'] },
  );
});

test('does not retry without a marker, with expired markers or for another release or attempt', async () => {
  for (const artifacts of [
    [],
    [{ id: 1, name: markerName, expired: true }],
    [{ id: 2, name: 'nightly-deferred-for-1234-2', expired: false }],
    [{ id: 3, name: 'nightly-deferred-for-123-1', expired: false }],
  ]) {
    const github = mockArtifacts([artifacts]);
    assert.deepEqual(
      await deferredNightly({ github, context, exec: github.exec, release: releaseAttempt }),
      { deferred: false, devices: null },
    );
  }
});

test('merges the devices of every run deferred for the release', async () => {
  const github = mockArtifacts([[
    { id: 7, name: markerName, expired: false },
    { id: 8, name: markerName, expired: false },
  ]], {
    7: { devices: ['a.local', 'b.local'] },
    8: { devices: ['b.local', 'c.local'] },
  });

  assert.deepEqual(
    await deferredNightly({ github, context, exec: github.exec, release: releaseAttempt }),
    { deferred: true, devices: ['a.local', 'b.local', 'c.local'] },
  );
});

test('retries every device when a run deferred before discovering them', async () => {
  const github = mockArtifacts([[
    { id: 7, name: markerName, expired: false },
    { id: 8, name: markerName, expired: false },
  ]], {
    7: { devices: ['a.local'] },
    8: { devices: null },
  });

  assert.deepEqual(
    await deferredNightly({ github, context, exec: github.exec, release: releaseAttempt }),
    { deferred: true, devices: null },
  );
});

test('finds a deferred nightly marker beyond the first artifact page', async () => {
  const github = mockArtifacts([
    Array.from({ length: 100 }, (_, index) => ({
      id: index, name: markerName, expired: true,
    })),
    [{ id: 100, name: markerName, expired: false }],
  ], { 100: { devices: null } });

  assert.deepEqual(
    await deferredNightly({ github, context, exec: github.exec, release: releaseAttempt }),
    { deferred: true, devices: null },
  );
  assert.deepEqual(github.requests.map(request => request.page), [1, 2]);
});

test('fails the nightly retry check when the artifact API fails', async () => {
  const github = mockArtifacts([]);
  const failure = new Error('Artifact API unavailable');
  github.paginate = async () => { throw failure; };

  await assert.rejects(
    deferredNightly({ github, context, exec: github.exec, release: releaseAttempt }),
    failure,
  );
});
