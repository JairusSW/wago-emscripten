#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include <unistd.h>

int main(int argc, char **argv) {
  unsigned char random[4096];
  int random_ok = 1;
  for (size_t offset = 0; offset < sizeof(random); offset += 256) {
    if (getentropy(random + offset, 256)) random_ok = 0;
  }
  struct timespec wall, monotonic;
  int clock_ok = !clock_gettime(CLOCK_REALTIME, &wall) && !clock_gettime(CLOCK_MONOTONIC, &monotonic);
  uint32_t varying = 0;
  for (size_t i = 1; i < sizeof(random); i++) varying |= random[i] ^ random[0];
  const char *value = getenv("WAGO_EMSCRIPTEN_FIXTURE");
  printf("system:%s:%s:%d:%d:%d\n",
         argc == 2 && argv[1] ? argv[1] : "bad-argv",
         value ? value : "missing-env", random_ok && varying != 0,
         clock_ok, clock_ok && wall.tv_sec > 0 && monotonic.tv_sec >= 0);
  return 0;
}
