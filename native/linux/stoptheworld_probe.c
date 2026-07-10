// stoptheworld_probe — a minimal, claude-free reproducer for the supervisor
// deadlock. It models JavaScriptCore's signal-based stop-the-world GC: a
// collector thread suspends every mutator by sending it SIGPWR via tgkill
// (si_code == SI_TKILL, thread-directed), waits for each to park inside its
// signal handler, does "GC", then resumes them — repeated for several rounds.
//
// Run bare, it finishes in milliseconds. Run under rcc_seccomp, the supervisor
// ptraces every thread; if its tracer loop does not propagate the SIGPWR
// handshake intact (the real bug), a suspend signal is lost and the collector
// waits forever. The harness bounds the run with a timeout and reads the exit
// status: clean exit 0 == green, timeout kill == the deadlock reproduced.
#define _GNU_SOURCE
#include <pthread.h>
#include <semaphore.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

#define NWORKERS 6
#define NROUNDS 200

static sem_t acked[NWORKERS];   // worker -> collector: "I am parked"
static sem_t resume_sem[NWORKERS]; // collector -> worker: "you may continue"
static sem_t resumed[NWORKERS]; // worker -> collector: "I have left the handler"
static volatile pid_t worker_tid[NWORKERS];
static _Thread_local int my_slot = -1;
static volatile int running = 1;

// The suspend handler: acknowledge parking, block until released, then confirm we
// are leaving. sem_post/sem_wait are async-signal-safe (POSIX), which is exactly
// why a runtime can park a mutator from inside a signal handler. The trailing
// `resumed` post lets the collector wait for the handler to fully unwind before
// it sends the next round's signal — standard signals do not queue, so without
// this a slow worker could collapse two rounds' SIGPWRs into one and desync the
// ack count. (This closes a race in the *probe*, not in the supervisor.)
static void on_suspend(int sig) {
	(void)sig;
	int s = my_slot;
	if (s < 0) return;
	sem_post(&acked[s]);
	sem_wait(&resume_sem[s]);
	sem_post(&resumed[s]);
}

static void *worker(void *arg) {
	long idx = (long)arg;
	my_slot = (int)idx;
	worker_tid[idx] = (pid_t)syscall(SYS_gettid);
	// Sleep in short bursts rather than hot-spin: the suspend signal interrupts
	// the nanosleep and runs the handler, which is the delivery path we want to
	// stress, without pegging a core per worker (which starves the collector and
	// blurs "deadlocked" into "merely slow").
	struct timespec ts = {.tv_sec = 0, .tv_nsec = 200000}; // 0.2 ms
	while (running) nanosleep(&ts, NULL);
	return NULL;
}

int main(void) {
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_handler = on_suspend;
	sigemptyset(&sa.sa_mask);
	sa.sa_flags = SA_RESTART;
	if (sigaction(SIGPWR, &sa, NULL) != 0) { perror("sigaction"); return 1; }

	for (int i = 0; i < NWORKERS; i++) {
		sem_init(&acked[i], 0, 0);
		sem_init(&resume_sem[i], 0, 0);
		sem_init(&resumed[i], 0, 0);
	}

	pthread_t th[NWORKERS];
	for (long i = 0; i < NWORKERS; i++) {
		if (pthread_create(&th[i], NULL, worker, (void *)i) != 0) { perror("pthread_create"); return 1; }
	}
	// Wait until every worker has published its tid.
	for (int i = 0; i < NWORKERS; i++)
		while (worker_tid[i] == 0) sched_yield();

	pid_t tgid = getpid();
	for (int r = 0; r < NROUNDS; r++) {
		// Suspend all mutators.
		for (int i = 0; i < NWORKERS; i++) {
			if (syscall(SYS_tgkill, tgid, worker_tid[i], SIGPWR) != 0) { perror("tgkill"); return 1; }
		}
		// Wait for each to park inside its handler.
		for (int i = 0; i < NWORKERS; i++) sem_wait(&acked[i]);
		// "GC" would run here, world stopped.
		// Resume all mutators and wait for each to fully leave its handler before
		// the next round, so a lost/collapsed signal cannot desync the count.
		for (int i = 0; i < NWORKERS; i++) sem_post(&resume_sem[i]);
		for (int i = 0; i < NWORKERS; i++) sem_wait(&resumed[i]);
	}

	running = 0;
	for (int i = 0; i < NWORKERS; i++) pthread_join(th[i], NULL);
	printf("STOPTHEWORLD_OK rounds=%d workers=%d\n", NROUNDS, NWORKERS);
	return 0;
}
