import { WendyHelloWorld } from '@wendylabs/core';

const message = `WendyHelloWorld() = ${WendyHelloWorld()}`;
console.log(message);
if (typeof document !== 'undefined') {
  document.getElementById('output').textContent = message;
}
