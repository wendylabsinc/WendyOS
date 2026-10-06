import { describe, expect, test } from 'claude-code/testing'

import { appIdFrom, readinessWindowFrom, wendyJsonPath } from '../hooks/project'

describe('wendyJsonPath', () => {
  test('an absolute project path is used as is', () => {
    expect(wendyJsonPath('/home/me', '/proj/app')).toBe('/proj/app/wendy.json')
  })
  test('a trailing slash is dropped', () => {
    expect(wendyJsonPath('/home/me', '/proj/app/')).toBe('/proj/app/wendy.json')
  })
  test('"." and "./x" resolve against the session directory', () => {
    expect(wendyJsonPath('/proj', '.')).toBe('/proj/wendy.json')
    expect(wendyJsonPath('/proj/', './app')).toBe('/proj/app/wendy.json')
  })
  test('".." is left for the filesystem to resolve', () => {
    expect(wendyJsonPath('/proj/app', '../other')).toBe('/proj/app/../other/wendy.json')
  })
  test('a Windows absolute path is absolute', () => {
    expect(wendyJsonPath('C:/Users/me', 'C:\\proj')).toBe('C:\\proj/wendy.json')
  })
})

describe('readinessWindowFrom', () => {
  test('is 0 without a readiness probe', () => {
    expect(readinessWindowFrom('{"appId":"a"}')).toBe(0)
    expect(readinessWindowFrom('not json')).toBe(0)
  })
  test('defaults to 30 s, as wendy run does', () => {
    expect(readinessWindowFrom('{"readiness":{"tcpSocket":{"port":8080}}}')).toBe(30)
    expect(readinessWindowFrom('{"readiness":{"tcpSocket":{"port":8080},"timeoutSeconds":0}}')).toBe(30)
  })
  test('uses timeoutSeconds when set', () => {
    expect(readinessWindowFrom('{"readiness":{"tcpSocket":{"port":8080},"timeoutSeconds":90}}')).toBe(90)
  })
})

describe('appIdFrom', () => {
  test('reads appId', () => {
    expect(appIdFrom('{"appId":"sh.wendy.demo"}')).toBe('sh.wendy.demo')
  })
  test('ignores a UTF-8 BOM', () => {
    expect(appIdFrom('\uFEFF{"appId":"sh.wendy.demo"}')).toBe('sh.wendy.demo')
  })
  test('is empty when absent or unreadable', () => {
    expect(appIdFrom('{"version":"1"}')).toBe('')
    expect(appIdFrom('{"appId": 7}')).toBe('')
    expect(appIdFrom('not json')).toBe('')
  })
})
