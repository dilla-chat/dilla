//! A fixed-size single-producer single-consumer ring of 16-bit samples: the hand-off between a
//! device's real-time audio callback and the threads that do the real work. Neither side allocates
//! or takes a lock; a producer that finds the ring full drops what does not fit (a stalled
//! consumer never blocks the callback), and the consumer may skip ahead.

use std::sync::atomic::{AtomicI16, AtomicUsize, Ordering};

pub struct Ring {
    buf: Box<[AtomicI16]>,
    mask: usize,
    /// Total samples ever written / read; their difference is the fill.
    head: AtomicUsize,
    tail: AtomicUsize,
}

impl Ring {
    /// A ring holding at least `capacity` samples (rounded up to a power of two).
    pub fn new(capacity: usize) -> Self {
        let cap = capacity.max(2).next_power_of_two();
        Self {
            buf: (0..cap).map(|_| AtomicI16::new(0)).collect(),
            mask: cap - 1,
            head: AtomicUsize::new(0),
            tail: AtomicUsize::new(0),
        }
    }

    pub fn len(&self) -> usize {
        self.head
            .load(Ordering::Acquire)
            .wrapping_sub(self.tail.load(Ordering::Acquire))
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Producer only. Writes as much of `samples` as fits and returns how many.
    pub fn push(&self, samples: &[i16]) -> usize {
        let head = self.head.load(Ordering::Relaxed);
        let tail = self.tail.load(Ordering::Acquire);
        let free = self.buf.len() - head.wrapping_sub(tail);
        let n = samples.len().min(free);
        for (i, s) in samples[..n].iter().enumerate() {
            self.buf[head.wrapping_add(i) & self.mask].store(*s, Ordering::Relaxed);
        }
        self.head.store(head.wrapping_add(n), Ordering::Release);
        n
    }

    /// Consumer only. Reads up to `out.len()` samples and returns how many.
    pub fn pop(&self, out: &mut [i16]) -> usize {
        let tail = self.tail.load(Ordering::Relaxed);
        let head = self.head.load(Ordering::Acquire);
        let n = out.len().min(head.wrapping_sub(tail));
        for (i, o) in out[..n].iter_mut().enumerate() {
            *o = self.buf[tail.wrapping_add(i) & self.mask].load(Ordering::Relaxed);
        }
        self.tail.store(tail.wrapping_add(n), Ordering::Release);
        n
    }

    /// Consumer only. Discards up to `n` of the oldest samples.
    pub fn skip(&self, n: usize) {
        let tail = self.tail.load(Ordering::Relaxed);
        let head = self.head.load(Ordering::Acquire);
        let n = n.min(head.wrapping_sub(tail));
        self.tail.store(tail.wrapping_add(n), Ordering::Release);
    }
}
