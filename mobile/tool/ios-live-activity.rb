# Injects the RouteActivity widget extension (lock-screen Live Activity)
# into the flutter-create-regenerated Xcode project, and adds the shared
# attributes + bridge sources to the Runner target. Idempotent — CI runs it
# on every build. Invoked by tool/ios-live-activity.sh, which also copies
# the Swift sources into place first.
require 'xcodeproj'

TEAM = '5582DW8S98'
EXT_NAME = 'RouteActivity'

proj = Xcodeproj::Project.open('ios/Runner.xcodeproj')
runner = proj.targets.find { |t| t.name == 'Runner' }
raise 'no Runner target' unless runner

# An embedded extension's bundle id MUST be prefixed with the parent app's,
# which differs between local builds (flutter-create default) and CI (pinned
# to com.checkmate…) — so derive it. CI must run this AFTER the pin step.
runner_bundle = runner.build_configurations
    .find { |c| c.name == 'Release' }
    &.build_settings&.[]('PRODUCT_BUNDLE_IDENTIFIER') ||
  'com.viam.viamChartplotterMobile'
ext_bundle_id = "#{runner_bundle}.#{EXT_NAME}"

# ---- Runner-side sources (shared attributes + method-channel bridge) ------
# The Runner group already carries the 'Runner' path prefix, so refs use the
# bare filename — 'Runner/x.swift' here resolved to Runner/Runner/x.swift.
runner_group = proj.main_group['Runner'] || proj.main_group
%w[RouteActivityAttributes.swift LiveActivityBridge.swift].each do |f|
  runner_group.files.select { |r| r.path == "Runner/#{f}" }
      .each(&:remove_from_project) # stale doubled-path refs
  ref = runner_group.files.find { |r| r.path == f }
  ref ||= runner_group.new_file(f)
  unless runner.source_build_phase.files_references.include?(ref)
    runner.add_file_references([ref])
  end
end

# ---- Extension target ------------------------------------------------------
ext = proj.targets.find { |t| t.name == EXT_NAME }
unless ext
  ext = proj.new_target(:app_extension, EXT_NAME, :ios, '16.2')

  group = proj.main_group.find_subpath(EXT_NAME, true)
  group.set_source_tree('<group>')
  group.set_path(EXT_NAME)
  srcs = %w[RouteActivityAttributes.swift RouteActivityWidget.swift].map do |f|
    group.new_file(f)
  end
  ext.add_file_references(srcs)

  # Embed in the app ("Embed Foundation Extensions" — dstSubfolderSpec 13).
  embed = runner.copy_files_build_phases.find { |p| p.name == 'Embed Foundation Extensions' }
  embed ||= runner.new_copy_files_build_phase('Embed Foundation Extensions')
  embed.dst_subfolder_spec = '13'
  bf = embed.add_file_reference(ext.product_reference)
  bf.settings = { 'ATTRIBUTES' => ['RemoveHeadersOnCopy'] }
  runner.add_dependency(ext)
end

# The embed phase must run BEFORE Flutter's "Thin Binary" script phase:
# that script takes the whole Runner.app as implicit input/output, so an
# embed appended after it forms a build cycle ("Cycle inside Runner").
embed = runner.copy_files_build_phases.find { |p| p.name == 'Embed Foundation Extensions' }
thin = runner.build_phases.find { |p| p.respond_to?(:name) && p.name == 'Thin Binary' }
if embed && thin
  phases = runner.build_phases
  if phases.index(embed) > phases.index(thin)
    phases.delete(embed)
    phases.insert(phases.index(thin), embed)
  end
end

# The appex's CFBundleVersion/CFBundleShortVersionString MUST match the
# app's exactly (ITMS-90473). The app's come from Flutter's Generated
# .xcconfig (FLUTTER_BUILD_NAME / FLUTTER_BUILD_NUMBER, driven by pubspec
# and --build-number), so the extension inherits the same file and
# references the same variables instead of hardcoding a version.
generated = proj.files.find { |f| f.path&.end_with?('Generated.xcconfig') }

ext.build_configurations.each do |c|
  c.base_configuration_reference ||= generated if generated
  bs = c.build_settings
  # Explicit: Flutter's project defines no project-level PRODUCT_NAME, and
  # xcodeproj's app_extension defaults leave it unset → the product built
  # as a nameless '.appex' and collided with its own create-directory step.
  bs['PRODUCT_NAME'] = EXT_NAME
  bs['PRODUCT_BUNDLE_IDENTIFIER'] = ext_bundle_id
  bs['INFOPLIST_FILE'] = "#{EXT_NAME}/Info.plist"
  bs['SWIFT_VERSION'] = '5.0'
  bs['IPHONEOS_DEPLOYMENT_TARGET'] = '16.2'
  bs['TARGETED_DEVICE_FAMILY'] = '1,2'
  bs['DEVELOPMENT_TEAM'] = TEAM
  bs['CODE_SIGN_STYLE'] = 'Automatic'
  bs['MARKETING_VERSION'] = '$(FLUTTER_BUILD_NAME)'
  bs['CURRENT_PROJECT_VERSION'] = '$(FLUTTER_BUILD_NUMBER)'
  bs['SKIP_INSTALL'] = 'YES'
end

proj.save
puts "route-activity: extension target + Runner sources wired"
