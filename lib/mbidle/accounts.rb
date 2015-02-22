module MBidle
  class Accounts < Array
    def self.read
      accounts = Accounts.new

      File.open(File.expand_path('~/.mbsyncrc'), 'r') do |f|
        f.each_line do |line|
          next if line =~ /^#/ || line =~ /^$/

          if (match = /^IMAPAccount (?<name>.*)$/.match(line))
            accounts << Account.new(match[:name])
          else
            accounts.last.read_config_line(line)
          end
        end
      end

      accounts
    end

    def for_path(path)
      find { |account| account.matches?(path) }
    end

    def for_paths(*paths)
      paths.map { |path| for_path(path) }.compact
    end
  end
end
