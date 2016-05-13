module MBidle
  class UniqueQueue < EM::Queue
    def push(*items)
      EM.schedule do
        items.each do |item|
          @sink.push(item) unless @sink.include?(item)
        end
        unless @popq.empty?
          @drain = @sink
          @sink = []
          @popq.shift.call @drain.shift until @drain.empty? || @popq.empty?
        end
      end
    end
  end
end
