module MBidle
  class UniqueQueue < EM::Queue
    def push(*items)
      EM.schedule do
        items.each do |item|
          @items.push(item) unless @items.include?(item)
        end
        @popq.shift.call @items.shift until @items.empty? || @popq.empty?
      end
    end
  end
end
