#include <stdio.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

int main(void) {
  char cwd[8];
  if (!getcwd(cwd, sizeof(cwd)) || cwd[0] != '/' || cwd[1] != 0) return 10;
  if (mkdir("/data", 0700)) return 11;
  FILE *file = fopen("/data/workload.tmp", "wb+");
  if (!file) return 12;
  for (int i = 0; i < 2000; i++) {
    fprintf(file, "row-%04d:%08x\n", i, i * 2654435761u);
  }
  long length = ftell(file);
  if (length <= 0 || ftruncate(fileno(file), length)) return 15;
  struct stat info;
  if (fstat(fileno(file), &info) || info.st_size != length || !S_ISREG(info.st_mode)) return 22;
  if (fseek(file, 0, SEEK_SET)) return 13;
  unsigned long checksum = 5381;
  int byte;
  while ((byte = fgetc(file)) != EOF) checksum = ((checksum << 5) + checksum) ^ (unsigned)byte;
  if (fclose(file)) return 14;
  if (rename("/data/workload.tmp", "/data/workload.txt")) return 16;
  if (access("/data/workload.txt", R_OK)) return 17;
	if (stat("/data/workload.txt", &info) || info.st_size != length) return 23;
	int fd = open("/data/workload.txt", O_RDWR);
	char *mapped = mmap(NULL, (size_t)length, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	if (fd < 0 || mapped == MAP_FAILED || mapped[0] != 'r') return 20;
	mapped[0] = 'R';
	if (munmap(mapped, (size_t)length) || close(fd)) return 21;
  fd = open("/data/workload.txt", O_RDWR);
  char first = 0, replacement = 'O';
  if (fd < 0 || pread(fd, &first, 1, 0) != 1 || first != 'R') return 24;
  if (pwrite(fd, &replacement, 1, 1) != 1 || fsync(fd)) return 25;
  if (posix_fallocate(fd, length, 128) || ftruncate(fd, length) || close(fd)) return 26;
  file = fopen("/data/workload.txt", "rb");
  if (!file || fgetc(file) != 'R' || fclose(file)) return 18;
  if (unlink("/data/workload.txt") || rmdir("/data")) return 19;
  printf("filesystem:%lu\n", checksum);
  return 0;
}
