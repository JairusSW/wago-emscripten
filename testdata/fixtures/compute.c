#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

static int compare(const void *left, const void *right) {
  int a = *(const int *)left;
  int b = *(const int *)right;
  return (a > b) - (a < b);
}

int main(int argc, char **argv) {
  const int count = 2500000;
  int *values = malloc((size_t)count * sizeof(*values));
  if (!values) return 10;
  for (int i = 0; i < count; i++) values[i] = (i * 48271) % 2147483647;
  qsort(values, count, sizeof(*values), compare);
  uint64_t checksum = 0;
  for (int i = 0; i < count; i += 997) checksum = checksum * 33 + (uint32_t)values[i];
  printf("compute:%d:%llu\n", argc, (unsigned long long)checksum);
  free(values);
  return 0;
}
