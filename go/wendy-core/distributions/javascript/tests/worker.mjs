import { parentPort } from 'node:worker_threads';
import * as core from '@wendylabs/core';
import { assertCore } from './conformance.mjs';
parentPort.postMessage(assertCore(core));
