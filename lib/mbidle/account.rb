module MBidle
  class Account
    attr_accessor :name, :host, :port, :user, :pass, :path, :folder, :cert, :imaps
    alias_method :imaps?, :imaps

    def initialize(name)
      @name = name
      @imaps = false
      @port = 993
    end

    def read_config_line(line)
      match = /^(?<key>[^ ]+) (?<value>.+)$/.match(line)
      case match[:key]
      when 'Host' then @host = match[:value]
      when 'User' then @user = match[:value]
      when 'Pass' then @pass = match[:value]
      when 'Path' then @path = match[:value]
      when 'Inbox' then @folder = File.basename(match[:value])
      when 'CertificateFile' then @cert = match[:value]
      when 'UseIMAPS' then @imaps = (match[:value] == 'yes')
      end
    end

    def matches?(given_path)
      !(/^#{path}/ !~ given_path)
    end

    def idle!
      Log.debug "Connect to #{host}:#{port}#{imaps? ? ' via IMAPS' : ''} for #{name}"
      Sync.schedule(self)

      @client = EM::IMAP.new(host, port, imaps?)
      @client
        .connect
        .bind!(&method(:login))
        .bind!(&method(:select_folder))
        .bind!(&method(:wait_for_new_emails))
        .errback(&method(:handle_error))
    rescue StandardError => e
      unless SHUTDOWN
        Log.warn "#{e} for #{name}. Waiting 5 minutes for reconnect ..."
        EM::Timer.new(300, &method(:idle!))
      end
    end

    def login(_results)
      Log.debug "Login as #{user}@#{host} for #{name}"
      @client.login(user, pass)
    end

    def select_folder(_results)
      Log.debug "Select #{folder} for #{name}"
      @client.select(folder)
    end

    def wait_for_new_emails(_results)
      @client.wait_for_new_emails do
        Sync.schedule(self)
      end
    end

    def handle_error(error)
      case "#{error}"
      when /unbound/
        unless SHUTDOWN
          Log.warn "#{host} connection unbound for #{name}. Waiting 5 minutes for reconnect ..."
          EM::Timer.new(300, &method(:idle!))
        end
      else
        Log.error "Some unidentified error: #{error.inspect}"
        Log.error error.backtrace
      end
    end
  end
end
