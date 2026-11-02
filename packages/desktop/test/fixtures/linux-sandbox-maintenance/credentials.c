#define _GNU_SOURCE
#include <stdio.h>
#include <sys/prctl.h>
#include <sys/types.h>
#include <unistd.h>
#ifndef DILLA_GENERATION
#error Compile an explicit CI fixture generation
#endif
int main(void) {
 uid_t r,e,s;gid_t gr,ge,gs;
 if(getresuid(&r,&e,&s)||getresgid(&gr,&ge,&gs))return 2;
 int nnp=prctl(PR_GET_NO_NEW_PRIVS,0,0,0,0);
 if(nnp<0)return 2;
 if(printf("CRED %u %u %u %u %u %u %d %u\n",(unsigned)r,(unsigned)e,(unsigned)s,(unsigned)gr,(unsigned)ge,(unsigned)gs,nnp,(unsigned)DILLA_GENERATION)<0)return 2;
 return fflush(stdout)?2:0;
}
