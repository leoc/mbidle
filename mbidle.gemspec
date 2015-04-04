# coding: utf-8
lib = File.expand_path('../lib', __FILE__)
$LOAD_PATH.unshift(lib) unless $LOAD_PATH.include?(lib)
require 'mbidle/version'

Gem::Specification.new do |spec|
  spec.name          = 'mbidle'
  spec.version       = MBidle::VERSION
  spec.authors       = ['Arthur Leonard Andersen']
  spec.email         = ['arthur@andersen.berlin']
  spec.summary       = %q{Listen to IMAP changes via IMAP idle.}
  spec.description   = %q{Listen to IMAP changes via IMAP idle.}
  spec.homepage      = 'http://github.com/leoc/mbidle'
  spec.license       = 'MIT'

  spec.files         = `git ls-files -z`.split("\x0")
  spec.executables   = spec.files.grep(%r{^bin/}) { |f| File.basename(f) }
  spec.test_files    = spec.files.grep(%r{^(test|spec|features)/})
  spec.require_paths = ['lib']

  spec.add_development_dependency 'bundler', '~> 1.7'
  spec.add_development_dependency 'rake', '~> 10.0'

  spec.add_dependency 'eventmachine'
  spec.add_dependency 'em-imap'
  spec.add_dependency 'em-dir-watcher'
  spec.add_dependency 'em-systemcommand'
  spec.add_dependency 'rb-inotify'
end
