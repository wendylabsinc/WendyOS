import { test } from 'node:test';
import { Worker } from 'node:worker_threads';
import * as core from '@wendylabs/core';
import { assertCore } from './conformance.mjs';

test('shared contract through the Node.js package export', () => assertCore(core));
test('the native backend also works in a worker', async () => {
  const worker = new Worker(new URL('./worker.mjs', import.meta.url));
  await new Promise((resolve, reject) => {
    worker.once('message', value => value === true ? resolve() : reject(new Error('Worker contract failed')));
    worker.once('error', reject);
    worker.once('exit', code => { if (code !== 0) reject(new Error(`Worker exited ${code}`)); });
  });
  await worker.terminate();
});
