require 'em-imap'
require 'em-dir-watcher'
require 'forwardable'
require 'em-systemcommand'

MBSYNC_COMMAND = ENV['MBSYNC_BIN'] || 'mbsync'

$LOAD_PATH << File.expand_path('lib/', File.dirname(__FILE__))

require 'mbidle/accounts'
require 'mbidle/account'
require 'mbidle/unique_queue'
require 'mbidle/sync'
require 'mbidle/log'

MBidle::Sync.after_sync do
  system 'bash /home/arthur/.bin/update-mail-widget'
end

SHUTDOWN = false

EM.run do
  @accounts = MBidle::Accounts.read

  EMDirWatcher.watch File.expand_path('~/.mail') do |paths|
    paths.map! { |p| File.join('~/.mail', p) }
    paths.reject! { |p| p =~ /\.mbsyncstate/ }
    accounts = @accounts.for_paths(*paths)
    system 'bash /home/arthur/.bin/update-mail-widget' unless accounts.empty?
    MBidle::Sync.schedule(*accounts)
  end

  @accounts.each(&:idle!)

  trap('TERM') do
    SHUTDOWN = true
    EM.stop
  end
end
