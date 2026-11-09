//! The emitter: one thread per process that streams NDJSON lines to Cortex.

/// One line, serialized twice: without and with its capture.
///
/// Each ends in `\n`. `row` holds `"capture":{"dropped":true}` when the line
/// has a capture, and no `capture` key otherwise.
pub struct Queued {
    pub row: Vec<u8>,
    pub with_capture: Option<Vec<u8>>,
}

use std::collections::VecDeque;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::Notify;

pub const MAX_QUEUE_LINES: usize = 4096;
pub const MAX_QUEUE_BYTES: usize = 256 << 20;
pub const MAX_LINE: usize = 64 << 20;

const BACKOFF_MIN: Duration = Duration::from_millis(100);
const BACKOFF_MAX: Duration = Duration::from_secs(5);

/// The emitter counters, and the queue size at the time of the call.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct EmitterStats {
    pub rows_emitted: u64,
    pub rows_dropped: u64,
    pub captures_dropped_line_cap: u64,
    pub captures_dropped_queue_bytes: u64,
    pub queued_lines: usize,
    pub queued_bytes: usize,
}

/// A queued line. `row` is the line without its capture, kept so that a later
/// row can take the capture's place in the queue.
struct Entry {
    line: Vec<u8>,
    row: Option<Vec<u8>>,
}

impl Entry {
    fn size(&self) -> usize {
        self.line.len() + self.row.as_ref().map_or(0, Vec::len)
    }
}

/// `bytes` counts each entry's line and its kept row: both are in memory.
#[derive(Default)]
struct Queue {
    lines: VecDeque<Entry>,
    bytes: usize,
}

impl Queue {
    /// Replaces queued captures with their rows, newest first, until `need`
    /// more bytes fit. Returns how many captures it dropped.
    fn drop_captures(&mut self, need: usize) -> u64 {
        let mut dropped = 0;
        for e in self.lines.iter_mut().rev() {
            if self.bytes + need <= MAX_QUEUE_BYTES {
                break;
            }
            if let Some(row) = e.row.take() {
                self.bytes -= e.line.len();
                e.line = row;
                dropped += 1;
            }
        }
        dropped
    }
}

/// Streams lines to `POST /v1/ingest` from its own thread. `submit` never
/// blocks: when the queue is full, it drops captures first, then lines.
pub struct Emitter {
    addr: SocketAddr,
    token: String,
    queue: Mutex<Queue>,
    wake: Notify,
    rows_emitted: AtomicU64,
    rows_dropped: AtomicU64,
    captures_dropped_line_cap: AtomicU64,
    captures_dropped_queue_bytes: AtomicU64,
}

fn bump(c: &AtomicU64, name: &'static str) {
    c.fetch_add(1, Ordering::Relaxed);
    metrics::counter!(name).increment(1);
}

fn bump_capture(c: &AtomicU64, reason: &'static str) {
    c.fetch_add(1, Ordering::Relaxed);
    metrics::counter!("cortex_captures_dropped_total", "reason" => reason).increment(1);
}

impl Emitter {
    /// Starts the sender thread. It runs for the life of the process.
    pub fn start(ingest_addr: SocketAddr, token: String) -> Arc<Emitter> {
        let e = Arc::new(Emitter {
            addr: ingest_addr,
            token,
            queue: Mutex::default(),
            wake: Notify::new(),
            rows_emitted: AtomicU64::new(0),
            rows_dropped: AtomicU64::new(0),
            captures_dropped_line_cap: AtomicU64::new(0),
            captures_dropped_queue_bytes: AtomicU64::new(0),
        });
        let sender = e.clone();
        std::thread::Builder::new()
            .name("cortex-emitter".into())
            .spawn(move || {
                tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("emitter runtime")
                    .block_on(sender.run());
            })
            .expect("emitter thread");
        e
    }

    /// Queues one line. It never blocks and never fails. A line that does not
    /// fit loses its capture first. Then queued lines lose their captures,
    /// newest first. Only then is the line dropped and counted.
    pub fn submit(&self, q: Queued) {
        let Queued { row, with_capture } = q;
        let mut entry = match with_capture {
            Some(c) if c.len() > MAX_LINE => {
                bump_capture(&self.captures_dropped_line_cap, "line_cap");
                Entry {
                    line: row,
                    row: None,
                }
            }
            Some(c) => Entry {
                line: c,
                row: Some(row),
            },
            None => Entry {
                line: row,
                row: None,
            },
        };
        let mut captures_dropped = 0;
        let queued = {
            let mut q = self.queue.lock().unwrap_or_else(|p| p.into_inner());
            if q.lines.len() >= MAX_QUEUE_LINES {
                false
            } else {
                if q.bytes + entry.size() > MAX_QUEUE_BYTES
                    && let Some(r) = entry.row.take()
                {
                    entry.line = r;
                    captures_dropped += 1;
                }
                captures_dropped += q.drop_captures(entry.size());
                let fits = q.bytes + entry.size() <= MAX_QUEUE_BYTES;
                if fits {
                    q.bytes += entry.size();
                    q.lines.push_back(entry);
                }
                fits
            }
        };
        for _ in 0..captures_dropped {
            bump_capture(&self.captures_dropped_queue_bytes, "queue_bytes");
        }
        if !queued {
            bump(&self.rows_dropped, "cortex_rows_dropped_total");
            return;
        }
        self.wake.notify_one();
    }

    pub fn stats(&self) -> EmitterStats {
        let (queued_lines, queued_bytes) = {
            let q = self.queue.lock().unwrap_or_else(|p| p.into_inner());
            (q.lines.len(), q.bytes)
        };
        EmitterStats {
            rows_emitted: self.rows_emitted.load(Ordering::Relaxed),
            rows_dropped: self.rows_dropped.load(Ordering::Relaxed),
            captures_dropped_line_cap: self.captures_dropped_line_cap.load(Ordering::Relaxed),
            captures_dropped_queue_bytes: self.captures_dropped_queue_bytes.load(Ordering::Relaxed),
            queued_lines,
            queued_bytes,
        }
    }

    fn pop(&self) -> Option<Vec<u8>> {
        let mut q = self.queue.lock().unwrap_or_else(|p| p.into_inner());
        let e = q.lines.pop_front()?;
        q.bytes -= e.size();
        Some(e.line)
    }

    /// Waits for the next line. Cancel-safe: the pop and the return are in
    /// one poll. `notify_one` keeps a permit, so no wake-up is lost.
    async fn next_line(&self) -> Vec<u8> {
        loop {
            if let Some(line) = self.pop() {
                return line;
            }
            self.wake.notified().await;
        }
    }

    async fn run(self: Arc<Self>) {
        let mut backoff = BACKOFF_MIN;
        loop {
            if self.stream(&mut backoff).await.is_err() {
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(BACKOFF_MAX);
            }
        }
    }

    /// One `POST /v1/ingest` with a chunked body, one chunk per line. It
    /// connects before it takes a line, so a failed connect drops nothing.
    // shortcut: HTTP/1.1 by hand, because hyper 1 needs hyper-util's TokioIo,
    // which is not a dependency; move to hyper when a crate brings it in.
    async fn stream(&self, backoff: &mut Duration) -> std::io::Result<()> {
        let mut conn = TcpStream::connect(self.addr).await?;
        conn.set_nodelay(true)?;
        let head = format!(
            "POST /v1/ingest HTTP/1.1\r\nHost: {}\r\nAuthorization: Bearer {}\r\n\
             Content-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n",
            self.addr, self.token
        );
        conn.write_all(head.as_bytes()).await?;
        let (mut rd, mut wr) = conn.split();
        let mut buf = [0u8; 512];
        loop {
            // Cortex answers only when the stream ends, so any read (a
            // response, EOF or an error) ends this stream.
            let line = tokio::select! {
                line = self.next_line() => line,
                _ = rd.read(&mut buf) => {
                    return Err(std::io::Error::other("ingest ended the stream"));
                }
            };
            let sent = async {
                wr.write_all(format!("{:x}\r\n", line.len()).as_bytes())
                    .await?;
                wr.write_all(&line).await?;
                wr.write_all(b"\r\n").await
            }
            .await;
            if let Err(err) = sent {
                bump(&self.rows_dropped, "cortex_rows_dropped_total");
                return Err(err);
            }
            bump(&self.rows_emitted, "cortex_rows_emitted_total");
            *backoff = BACKOFF_MIN;
        }
    }
}

/// The emitter of this process. The first call starts it; a later call with
/// other arguments gets the same emitter, and a warning is logged.
pub fn global(ingest_addr: SocketAddr, token: String) -> Arc<Emitter> {
    static GLOBAL: OnceLock<Arc<Emitter>> = OnceLock::new();
    let e = GLOBAL.get_or_init(|| Emitter::start(ingest_addr, token.clone()));
    if e.addr != ingest_addr || e.token != token {
        eprintln!(
            "cortex-gateway: the emitter already streams to {}; ignoring a second ingest config for {ingest_addr}",
            e.addr
        );
    }
    e.clone()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Read};
    use std::net::TcpListener;
    use std::sync::mpsc;
    use std::time::{Duration, Instant};

    fn line(s: &str) -> Queued {
        Queued {
            row: format!("{s}\n").into_bytes(),
            with_capture: None,
        }
    }

    /// An address with nothing listening on it.
    fn closed_addr() -> SocketAddr {
        TcpListener::bind("127.0.0.1:0")
            .unwrap()
            .local_addr()
            .unwrap()
    }

    /// Reads one ingest request: sends its Authorization header, then each
    /// chunk. Returns after `max` chunks, which closes the connection.
    fn read_stream(conn: std::net::TcpStream, tx: &mpsc::Sender<String>, max: usize) {
        let mut r = BufReader::new(conn);
        let mut l = String::new();
        loop {
            l.clear();
            if r.read_line(&mut l).unwrap_or(0) == 0 {
                return;
            }
            if l == "\r\n" {
                break;
            }
            if let Some(v) = l.strip_prefix("Authorization: ") {
                tx.send(format!("auth {}", v.trim_end())).unwrap();
            }
        }
        for _ in 0..max {
            l.clear();
            if r.read_line(&mut l).unwrap_or(0) == 0 {
                return;
            }
            let n = usize::from_str_radix(l.trim_end(), 16).unwrap();
            let mut data = vec![0; n + 2];
            if r.read_exact(&mut data).is_err() {
                return;
            }
            data.truncate(n);
            let mut s = String::from_utf8(data).unwrap();
            if s.len() > 64 {
                s = format!("{} bytes", s.len());
            }
            tx.send(s).unwrap();
        }
    }

    /// A mock ingest server. Each accepted connection is read for at most
    /// `per_conn[i]` chunks (the last value repeats).
    fn serve(per_conn: Vec<usize>) -> (SocketAddr, mpsc::Receiver<String>) {
        let ln = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = ln.local_addr().unwrap();
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            for (i, conn) in ln.incoming().enumerate() {
                let max = *per_conn.get(i).or(per_conn.last()).unwrap();
                read_stream(conn.unwrap(), &tx, max);
            }
        });
        (addr, rx)
    }

    fn recv(rx: &mpsc::Receiver<String>) -> String {
        rx.recv_timeout(Duration::from_secs(10)).unwrap()
    }

    #[test]
    fn lines_arrive_in_order_with_token() {
        let (addr, rx) = serve(vec![usize::MAX]);
        let e = Emitter::start(addr, "t".into());
        for s in ["a", "b", "c"] {
            e.submit(line(s));
        }
        assert_eq!(recv(&rx), "auth Bearer t");
        for s in ["a\n", "b\n", "c\n"] {
            assert_eq!(recv(&rx), s);
        }
        assert_eq!(e.stats().rows_emitted, 3);
    }

    #[test]
    fn capture_over_line_cap_is_dropped_row_kept() {
        let (addr, rx) = serve(vec![usize::MAX]);
        let e = Emitter::start(addr, "t".into());
        e.submit(Queued {
            row: b"row\n".to_vec(),
            with_capture: Some(vec![b'x'; MAX_LINE + 1]),
        });
        assert_eq!(recv(&rx), "auth Bearer t");
        assert_eq!(recv(&rx), "row\n");
        assert_eq!(e.stats().captures_dropped_line_cap, 1);
    }

    #[test]
    fn queue_drops_captures_before_rows() {
        let e = Emitter::start(closed_addr(), "t".into());
        for _ in 0..30 {
            e.submit(Queued {
                row: b"row\n".to_vec(),
                with_capture: Some(vec![b'x'; 10 << 20]),
            });
        }
        let s = e.stats();
        assert!(s.queued_bytes <= MAX_QUEUE_BYTES, "{}", s.queued_bytes);
        assert!(s.captures_dropped_queue_bytes > 0);
        assert_eq!(s.rows_dropped, 0);
        assert_eq!(s.queued_lines, 30);
    }

    #[test]
    fn queued_captures_make_room_for_a_row() {
        let e = Emitter::start(closed_addr(), "t".into());
        for _ in 0..25 {
            e.submit(Queued {
                row: b"row\n".to_vec(),
                with_capture: Some(vec![b'x'; 10 << 20]),
            });
        }
        assert_eq!(e.stats().captures_dropped_queue_bytes, 0);
        // A 15 MiB row with no capture: the newest queued capture gives it room.
        e.submit(Queued {
            row: vec![b'r'; 15 << 20],
            with_capture: None,
        });
        let s = e.stats();
        assert_eq!(s.rows_dropped, 0);
        assert_eq!(s.captures_dropped_queue_bytes, 1);
        assert_eq!(s.queued_lines, 26);
        assert!(s.queued_bytes <= MAX_QUEUE_BYTES, "{}", s.queued_bytes);
    }

    #[test]
    fn line_limit_drops_whole_lines() {
        let e = Emitter::start(closed_addr(), "t".into());
        for _ in 0..=MAX_QUEUE_LINES {
            e.submit(line("x"));
        }
        let s = e.stats();
        assert_eq!(s.rows_dropped, 1);
        assert_eq!(s.queued_lines, MAX_QUEUE_LINES);
    }

    #[test]
    fn submit_never_blocks_when_ingest_is_down() {
        let e = Emitter::start(closed_addr(), "t".into());
        let t = Instant::now();
        for _ in 0..1000 {
            e.submit(line("x"));
        }
        assert!(t.elapsed() < Duration::from_secs(1), "{:?}", t.elapsed());
    }

    #[test]
    fn reconnects_after_ingest_restarts() {
        let (addr, rx) = serve(vec![1, usize::MAX]);
        let e = Emitter::start(addr, "t".into());
        e.submit(line("1"));
        assert_eq!(recv(&rx), "auth Bearer t");
        assert_eq!(recv(&rx), "1\n");
        // The server closed the stream. Line 2 can be lost; line 3 must arrive.
        std::thread::sleep(Duration::from_millis(100));
        e.submit(line("2"));
        std::thread::sleep(Duration::from_millis(300));
        e.submit(line("3"));
        let t = Instant::now();
        while recv(&rx) != "3\n" {
            assert!(t.elapsed() < Duration::from_secs(10));
        }
    }
}
