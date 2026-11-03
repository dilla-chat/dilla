#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#ifndef DILLA_CI_ROOT
#define DILLA_CI_ROOT "/usr/lib/dilla-sandbox-maintenance-ci"
#endif
#ifndef DILLA_CI_LEASE_UID
#define DILLA_CI_LEASE_UID 0
#endif
#ifndef DILLA_CI_LEASE_GID
#define DILLA_CI_LEASE_GID 0
#endif
int main(void) {
 /* FD3 carries flock authority despite being read-only. Fixed helpers retain
    it without unlock/close until process exit; never expose it to other code. */
 struct stat lease;
 if(fstat(3,&lease)||!S_ISREG(lease.st_mode)||lease.st_uid!=DILLA_CI_LEASE_UID||lease.st_gid!=DILLA_CI_LEASE_GID||lease.st_nlink!=1||lease.st_size!=0||(lease.st_mode&07777)!=0600)return 2;
 int rights=fcntl(3,F_GETFL),fdflags=fcntl(3,F_GETFD);
 if(rights<0||(rights&O_ACCMODE)!=O_RDONLY||fdflags<0||(fdflags&FD_CLOEXEC))return 2;

 if(getuid()==0||geteuid()!=getuid())return 2;
 int fd=open(DILLA_CI_ROOT "/appimage/chrome-sandbox",O_RDONLY|O_CLOEXEC|O_NOFOLLOW);
 struct stat st;
 if(fd<0||fstat(fd,&st)||!S_ISREG(st.st_mode)||st.st_nlink!=1)return 2;
 int groups=getgroups(0,NULL),nnp=prctl(PR_GET_NO_NEW_PRIVS,0,0,0,0);
 if(groups<0||nnp<0)return 2;
 uid_t ru,eu,su;gid_t rg,eg,sg;
 if(getresuid(&ru,&eu,&su)||getresgid(&rg,&eg,&sg))return 2;
 printf("HELD %ju %ju %u %u %u %u %u %u %d %d\n",(uintmax_t)st.st_dev,(uintmax_t)st.st_ino,(unsigned)ru,(unsigned)eu,(unsigned)su,(unsigned)rg,(unsigned)eg,(unsigned)sg,groups,nnp);
 if(fflush(stdout))return 2;
 unsigned char command;
 while(read(STDIN_FILENO,&command,1)==1){
  if(command=='Q')break;
  if(command!='E')return 2;
  pid_t child=fork();if(child<0)return 2;
  if(child==0){
   char *const argv[]={"credential",NULL};char *const env[]={"LANG=C","LC_ALL=C",NULL};
   (void)syscall(SYS_execveat,fd,"",argv,env,AT_EMPTY_PATH);
   _exit(3);
  }
  int status=0,done=0;
  for(int i=0;i<200;i++){
   pid_t found=waitpid(child,&status,WNOHANG);
   if(found==child){done=1;break;}
   if(found<0&&errno!=EINTR)return 2;
   struct timespec pause={0,10000000};(void)nanosleep(&pause,NULL);
  }
  if(!done){(void)kill(child,SIGKILL);(void)waitpid(child,&status,0);return 2;}
  if(!WIFEXITED(status)||WEXITSTATUS(status)!=0)return 2;
 }
 return close(fd)?2:0;
}
