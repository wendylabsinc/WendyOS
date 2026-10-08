import { describe, expect, test } from 'claude-code/testing'

import { leadingJson } from '../hooks/json'

describe('leadingJson', () => {
  test('reads an object followed by more text', () => {
    expect(leadingJson('{"a":1}\nA new Wendy CLI version is available')).toEqual({ value: { a: 1 }, rest: '\nA new Wendy CLI version is available' })
  })
  test('is not fooled by braces and quotes inside strings', () => {
    expect(leadingJson('{"a":"}{\\"","b":{"c":[1,{}]}} tail')).toEqual({ value: { a: '}{"', b: { c: [1, {}] } }, rest: ' tail' })
  })
  test('allows leading whitespace and a BOM', () => {
    expect(leadingJson('\uFEFF  {"a":1}')).toEqual({ value: { a: 1 }, rest: '' })
  })
  test('refuses what does not start with an object', () => {
    expect(leadingJson('[1]')).toBeUndefined()
    expect(leadingJson('hello {"a":1}')).toBeUndefined()
    expect(leadingJson('{"a":')).toBeUndefined()
  })
})
