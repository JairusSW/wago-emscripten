#include <setjmp.h>
#include <stdio.h>

static jmp_buf target;

static void descend(int depth) {
  if (!depth) longjmp(target, 73);
  descend(depth - 1);
}

int main(void) {
  int result = setjmp(target);
  if (!result) descend(200);
  printf("setjmp:%d\n", result);
  return result == 73 ? 0 : 40;
}
