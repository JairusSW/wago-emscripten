#include <stdio.h>
#include <time.h>

int main(void) {
  time_t stamp = 1704067200;
  struct tm *value = gmtime(&stamp);
  char output[64];
  if (!value || !strftime(output, sizeof(output), "%Y-%m-%d %H:%M:%S", value)) return 11;
  printf("time:%s\n", output);
  return 0;
}
