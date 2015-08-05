require 'logger'

module MBidle
  class Log
    class << self
      def logger
        @logger ||= Logger.new(STDOUT)
      end

      def method_missing(*args)
        logger.send(*args)
      end
    end
  end
end
