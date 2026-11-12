//! group-queue.ts `SerialQueues`: one FIFO lane per key (`g:<group>`, the welcome lane `w`, `c:<channel>`), so every
//! apply, catch-up, commit and send of a group runs one at a time and in the order it was asked for; `coalesce` drops a
//! job whose tag already waits (not yet started) in its lane. A job's place is taken when it is asked for (as TS chains
//! onto the lane's tail synchronously), not when its task first runs.

use std::collections::{HashMap, HashSet, VecDeque};
use std::future::Future;
use std::sync::{Arc, Mutex};

use tokio::sync::oneshot;

#[derive(Default)]
struct Lane {
    busy: bool,
    waiters: VecDeque<oneshot::Sender<()>>,
    /// Tags of coalesced jobs queued and not yet started.
    tags: HashSet<&'static str>,
    /// Jobs queued or running.
    count: usize,
}

#[derive(Default)]
pub struct Lanes {
    lanes: Mutex<HashMap<String, Lane>>,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// A job's turn on its lane, released (to the next job in order) when dropped.
struct Turn<'a> {
    lanes: &'a Lanes,
    key: String,
}

impl Drop for Turn<'_> {
    fn drop(&mut self) {
        let mut lanes = lock(&self.lanes.lanes);
        let Some(lane) = lanes.get_mut(&self.key) else {
            return;
        };
        lane.count -= 1;
        loop {
            match lane.waiters.pop_front() {
                // A waiter that is gone (its task was dropped) gives its place to the next.
                Some(next) => {
                    if next.send(()).is_ok() {
                        break;
                    }
                    lane.count -= 1;
                }
                None => {
                    lane.busy = false;
                    break;
                }
            }
        }
        if lane.count == 0 {
            lanes.remove(&self.key);
        }
    }
}

impl Lanes {
    /// Takes a place on `key` now; the receiver resolves when it is this job's turn (`None`: at once).
    fn queue(&self, key: &str, tag: Option<&'static str>) -> Option<Option<oneshot::Receiver<()>>> {
        let mut lanes = lock(&self.lanes);
        let lane = lanes.entry(key.to_owned()).or_default();
        if let Some(tag) = tag
            && !lane.tags.insert(tag)
        {
            return None;
        }
        lane.count += 1;
        if lane.busy {
            let (tx, rx) = oneshot::channel();
            lane.waiters.push_back(tx);
            Some(Some(rx))
        } else {
            lane.busy = true;
            Some(None)
        }
    }

    /// Runs `job` after every job queued on `key` before it. A job must never wait on its own lane (it would wait for
    /// itself), as in TS.
    pub async fn run<T>(&self, key: &str, job: impl Future<Output = T>) -> T {
        // A job dropped while it waits is skipped by the job ahead of it (its sender fails); one that has its turn holds
        // the `Turn`, which hands the lane on however the job ends.
        if let Some(rx) = self.queue(key, None).flatten() {
            let _ = rx.await;
        }
        let turn = Turn {
            lanes: self,
            key: key.to_owned(),
        };
        let out = job.await;
        drop(turn);
        out
    }

    /// Queues `job` on `key` unless a job with `tag` is already queued there and has not started; answers whether it
    /// was queued. The job runs on its own task.
    pub fn coalesce<F>(self: &Arc<Self>, key: &str, tag: &'static str, job: F) -> bool
    where
        F: Future<Output = ()> + Send + 'static,
    {
        self.queue_job(key, Some(tag), job)
    }

    /// Queues `job` on `key` now (its place is taken before this returns) and runs it on its own task: how a frame
    /// handler hands work to a group's lane without waiting for it.
    pub fn spawn<F>(self: &Arc<Self>, key: &str, job: F)
    where
        F: Future<Output = ()> + Send + 'static,
    {
        self.queue_job(key, None, job);
    }

    fn queue_job<F>(self: &Arc<Self>, key: &str, tag: Option<&'static str>, job: F) -> bool
    where
        F: Future<Output = ()> + Send + 'static,
    {
        let Some(place) = self.queue(key, tag) else {
            return false;
        };
        let this = Arc::clone(self);
        let key = key.to_owned();
        tokio::spawn(async move {
            if let Some(rx) = place {
                let _ = rx.await;
            }
            let turn = Turn {
                lanes: &this,
                key: key.clone(),
            };
            if let Some(tag) = tag
                && let Some(lane) = lock(&this.lanes).get_mut(&key)
            {
                lane.tags.remove(tag);
            }
            job.await;
            drop(turn);
        });
        true
    }

    /// Whether nothing is queued or running on `key`.
    pub fn idle(&self, key: &str) -> bool {
        !lock(&self.lanes).contains_key(key)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[tokio::test]
    async fn one_job_at_a_time_in_order_and_coalesced() {
        let lanes = Arc::new(Lanes::default());
        let log = Arc::new(Mutex::new(Vec::new()));
        let gate = Arc::new(tokio::sync::Notify::new());
        // A long first job holds the lane; two catch-ups behind it coalesce into one; a run after them waits its turn.
        let (l, g) = (log.clone(), gate.clone());
        assert!(lanes.coalesce("g:1", "first", async move {
            g.notified().await;
            lock(&l).push("first");
        }));
        let l = log.clone();
        assert!(lanes.coalesce("g:1", "catch-up", async move { lock(&l).push("catch-up") }));
        let l = log.clone();
        assert!(!lanes.coalesce("g:1", "catch-up", async move { lock(&l).push("twice") }));
        let l = log.clone();
        let other = lanes.clone();
        let run =
            tokio::spawn(
                async move { other.run("g:1", async move { lock(&l).push("run") }).await },
            );
        // Another lane is not held up.
        lanes.run("g:2", async {}).await;
        tokio::time::sleep(Duration::from_millis(10)).await;
        assert!(lock(&log).is_empty());
        gate.notify_one();
        run.await.unwrap();
        assert_eq!(*lock(&log), ["first", "catch-up", "run"]);
        assert!(lanes.idle("g:1") && lanes.idle("g:2"));
    }
}
