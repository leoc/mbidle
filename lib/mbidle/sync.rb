require 'pry'

module MBidle
  class Sync
    class << self
      def queue
        @queue ||= begin
                     queue = UniqueQueue.new
                     queue.pop(&method(:run))
                     queue
                   end
      end

      def run(account_name)
        command = "#{MBSYNC_COMMAND} #{account_name}"
        Log.debug "Run #{command}"
        EM::SystemCommand.execute(command) do |on|
          on.failure(&method(:failed))
          on.success(&method(:succeeded))
        end
      end

      def failed(process)
        Log.warn "#{process.command} failed with status #{process.status.exitstatus}"
        Log.warn process.stderr.output
        @queue.pop(&method(:run))
      end

      def succeeded(process)
        Log.debug "#{process.command} finished."
        @queue.pop(&method(:run))
        after_sync_callbacks.each(&:call)
      end

      def schedule(*accounts)
        names = accounts.compact.map(&:name).uniq
        return if names.empty?

        queue.push(*names)
      end

      def after_sync_callbacks
        @aftersync_callbacks ||= []
      end

      def after_sync(&block)
        after_sync_callbacks << block
      end
    end
  end
end
