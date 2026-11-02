#define _GNU_SOURCE
#include <stdio.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <sys/prctl.h>
#include <sys/types.h>
#include <unistd.h>
#ifndef DILLA_GENERATION
#error Compile an explicit CI fixture generation
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

 uid_t r,e,s;gid_t gr,ge,gs;
 if(getresuid(&r,&e,&s)||getresgid(&gr,&ge,&gs))return 2;
 int nnp=prctl(PR_GET_NO_NEW_PRIVS,0,0,0,0);
 if(nnp<0)return 2;
 if(printf("CRED %u %u %u %u %u %u %d %u\n",(unsigned)r,(unsigned)e,(unsigned)s,(unsigned)gr,(unsigned)ge,(unsigned)gs,nnp,(unsigned)DILLA_GENERATION)<0)return 2;
 return fflush(stdout)?2:0;
}
