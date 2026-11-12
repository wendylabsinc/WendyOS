#!/usr/bin/env ruby
# Derive fixture build settings and production sources from the shipping target.
# Test entry points and launch controls never enter a shipping scheme.
require 'yaml'
require 'json'
require 'fileutils'

root = File.expand_path('..', __dir__)
spec = YAML.load_file(File.join(root, 'project.yml'))
fixture = spec.fetch('targets').fetch('WendyDesktop')
fixture['sources'] << { 'path' => 'MenuUITests/Fixture' }
fixture['dependencies'].reject! { |dependency| dependency.key?('target') }
fixture['settings']['base'].merge!({
  'PRODUCT_BUNDLE_IDENTIFIER' => 'dev.wendy.regression.menufixture',
  'PRODUCT_NAME' => 'WendyMenuFixture',
  'CODE_SIGN_ENTITLEMENTS' => '',
  'SWIFT_ACTIVE_COMPILATION_CONDITIONS' => '$(inherited) WENDY_MENU_UI_TESTS'
})
spec['name'] = 'WendyMenuUI'
spec['targets'] = {
  'WendyMenuFixture' => fixture,
  'WendyMenuUITests' => {
    'type' => 'bundle.ui-testing', 'platform' => 'macOS',
    'sources' => ['MenuUITests/Tests'],
    'dependencies' => [{ 'target' => 'WendyMenuFixture' }],
    'settings' => { 'base' => {
      'GENERATE_INFOPLIST_FILE' => 'YES',
      'PRODUCT_BUNDLE_IDENTIFIER' => 'dev.wendy.regression.menuuitests',
      'TEST_TARGET_NAME' => 'WendyMenuFixture'
    } }
  }
}
spec['schemes'] = { 'WendyMenuUI' => {
  'build' => { 'targets' => { 'WendyMenuFixture' => 'all' } },
  'test' => { 'targets' => ['WendyMenuUITests'], 'parallelizable' => false }
} }
FileUtils.mkdir_p(File.join(root, 'Build', 'MenuUI'))
File.write(File.join(root, 'Build', 'MenuUI', 'project.json'), JSON.pretty_generate(spec))
