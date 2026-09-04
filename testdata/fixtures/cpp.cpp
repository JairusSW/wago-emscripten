#include <algorithm>
#include <cstdio>
#include <numeric>
#include <memory>
#include <vector>

struct transform {
  virtual ~transform() = default;
  virtual long long apply(long long value) const = 0;
};

struct identity final : transform {
  long long apply(long long value) const override { return value; }
};

int main() {
  std::vector<long long> values(100000);
  for (size_t i = 0; i < values.size(); i++) values[i] = (i * 7919) % 104729;
  std::sort(values.begin(), values.end());
  std::unique_ptr<transform> operation = std::make_unique<identity>();
  std::printf("cpp:%lld\n", operation->apply(std::accumulate(values.begin(), values.end(), 0LL)));
  return 0;
}
