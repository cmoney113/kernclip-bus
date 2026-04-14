/**
 * kernclip-bus.js — KernClip Bus SDK for Node.js
 * Zero dependencies. stdlib only.
 *
 * Usage:
 *   const { Bus } = require('./kernclip-bus');
 *   const bus = new Bus();
 *   await bus.pub('my-topic', 'hello world');
 *   const msg = await bus.get('my-topic');
 *   bus.sub('my-topic', (msg) => console.log(msg));
 */

'use strict';

const net  = require('net');
const os   = require('os');
const path = require('path');

function defaultSocket() {
  return `/run/user/${process.getuid()}/kernclip-bus.sock`;
}

class BusError extends Error {}

class Message {
  constructor(d) {
    this.topic  = d.topic  ?? '';
    this.mime   = d.mime   ?? 'text/plain';
    this.data   = d.data   ?? '';
    this.seq    = d.seq    ?? 0;
    this.ts     = d.ts     ?? 0;
    this.sender = d.sender ?? '';
  }
  toString() { return `Message(${this.topic}#${this.seq}: ${this.data})`; }
}

class Bus {
  constructor(socketPath) {
    this.socketPath = socketPath ?? defaultSocket();
  }

  _connect() {
    return new Promise((resolve, reject) => {
      const sock = net.createConnection(this.socketPath);
      sock.once('connect', () => resolve(sock));
      sock.once('error', (e) => reject(
        new BusError(`kernclip-busd not running? ${e.message}\nSocket: ${this.socketPath}`)
      ));
    });
  }

  async _roundtrip(msg) {
    const sock = await this._connect();
    return new Promise((resolve, reject) => {
      let buf = '';
      sock.on('data', (chunk) => {
        buf += chunk.toString();
        const nl = buf.indexOf('\n');
        if (nl !== -1) {
          sock.destroy();
          try { resolve(JSON.parse(buf.slice(0, nl))); }
          catch (e) { reject(e); }
        }
      });
      sock.on('error', reject);
      sock.write(JSON.stringify(msg) + '\n');
    });
  }

  /** Publish data to a topic. Returns assigned seq number. */
  async pub(topic, data, mime = 'text/plain') {
    const r = await this._roundtrip({ op: 'pub', topic, mime, data });
    if (!r.ok) throw new BusError(r.error ?? 'unknown error');
    return r.seq;
  }

  /**
   * Get the latest message on a topic, or null if empty.
   * Pass afterSeq to fetch all messages newer than that seq.
   */
  async get(topic, afterSeq = undefined) {
    const req = { op: 'get', topic };
    if (afterSeq !== undefined) req.after_seq = afterSeq;
    const r = await this._roundtrip(req);
    if (!r.ok) throw new BusError(r.error ?? 'unknown error');
    if (afterSeq !== undefined) return (r.messages ?? []).map(m => new Message(m));
    if (!r.data && !r.seq) return null;
    return new Message(r);
  }

  /**
   * Subscribe to a topic. Calls callback(msg) for each new message.
   * Returns a close() function to stop the subscription.
   *
   * Example:
   *   const unsub = bus.sub('ai.responses', msg => console.log(msg.data));
   *   // later:
   *   unsub();
   */
  sub(topic, callback) {
    let sock;
    let buf = '';
    let closed = false;

    this._connect().then((s) => {
      sock = s;
      sock.write(JSON.stringify({ op: 'sub', topic }) + '\n');
      sock.on('data', (chunk) => {
        buf += chunk.toString();
        let nl;
        while ((nl = buf.indexOf('\n')) !== -1) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (line) {
            try {
              const r = JSON.parse(line);
              if (r.ok) callback(new Message(r));
            } catch (_) {}
          }
        }
      });
      sock.on('close', () => { if (!closed) closed = true; });
      sock.on('error', () => {});
    }).catch(() => {});

    return function close() {
      closed = true;
      if (sock) sock.destroy();
    };
  }


  /** Subscribe to topics matching a glob pattern. */



  /** Subscribe to topics matching a glob pattern. */
  subPattern(pattern, callback) {
    let sock;
    let buf = '';
    let closed = false;

    this._connect().then((s) => {
      sock = s;
      sock.write(JSON.stringify({ op: 'sub', pattern }) + '\n');
      sock.on('data', (chunk) => {
        buf += chunk.toString();
        let nl;
        while ((nl = buf.indexOf('\n')) !== -1) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (line) {
            try {
              const r = JSON.parse(line);
              if (r.ok) callback(new Message(r));
            } catch (_) {}
          }
        }
      });
      sock.on('close', () => { if (!closed) closed = true; });
      sock.on('error', () => {});
    }).catch(() => {});

    return function close() {
      closed = true;
      if (sock) sock.destroy();
    };
  }

  /** Subscribe to multiple topics (fan-in). */
  subMulti(topics, callback) {
    let sock;
    let buf = '';
    let closed = false;

    this._connect().then((s) => {
      sock = s;
      sock.write(JSON.stringify({ op: 'sub', topics }) + '\n');
      sock.on('data', (chunk) => {
        buf += chunk.toString();
        let nl;
        while ((nl = buf.indexOf('\n')) !== -1) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (line) {
            try {
              const r = JSON.parse(line);
              if (r.ok) callback(new Message(r));
            } catch (_) {}
          }
        }
      });
      sock.on('close', () => { if (!closed) closed = true; });
      sock.on('error', () => {});
    }).catch(() => {});

    return function close() {
      closed = true;
      if (sock) sock.destroy();
    };
  }
  async createTopic(topic, ringSize) {


    const req = { op: 'create_topic', topic };


    if (ringSize !== undefined) req.ring_size = ringSize;


    return await this._roundtrip(req);


  }


  


  /** Get daemon manifest (self-describing schema). */


  async manifest() {


    return await this._roundtrip({ op: 'manifest' });


  }


  /** List all active topics. */
  async topics() {
    const r = await this._roundtrip({ op: 'topics' });
    return r.topics ?? [];
  }

  async getClipboard() {
    const msg = await this.get('kernclip.clipboard');
    return msg ? msg.data : null;
  }
  async setClipboard(text) {
    return this.pub('kernclip.clipboard', text, 'text/plain');
  }

  // ── Binary / file helpers ──────────────────────────────────────────────────

  /**
   * Publish a Buffer as a base64 file envelope.
   * @param {string} topic
   * @param {Buffer} buf
   * @param {string} filename  Original filename (used for MIME detection and by receivers)
   * @param {string} [mime]    Override MIME type
   */
  async pubBytes(topic, buf, filename = 'data.bin', mime) {
    if (!mime) {
      mime = _mimeFromFilename(filename);
    }
    const envelope = JSON.stringify({
      encoding: 'base64',
      filename,
      size: buf.length,
      data: buf.toString('base64'),
    });
    return this.pub(topic, envelope, mime);
  }

  /**
   * Read a file from disk and publish it as a base64 envelope.
   * @param {string} topic
   * @param {string} filePath
   * @param {string} [mime]
   */
  async pubFile(topic, filePath, mime) {
    const fs = require('fs');
    const path = require('path');
    const buf = fs.readFileSync(filePath);
    const filename = path.basename(filePath);
    return this.pubBytes(topic, buf, filename, mime);
  }

  /**
   * Get the latest message and decode it as a file envelope.
   * Returns { buf, filename, mime } or null.
   */
  async getBytes(topic) {
    const msg = await this.get(topic);
    if (!msg) return null;
    return _decodeFileEnvelope(msg);
  }

  /**
   * Get the latest file payload from a topic and save it to disk.
   * If dest is a directory (or omitted → cwd), the original filename is used.
   * Returns the saved path, or null if no file payload.
   * @param {string} topic
   * @param {string} [dest]  Destination path or directory
   */
  async getFile(topic, dest) {
    const fs   = require('fs');
    const path = require('path');
    const result = await this.getBytes(topic);
    if (!result) return null;
    const { buf, filename } = result;
    let out = dest || filename;
    if (fs.existsSync(out) && fs.statSync(out).isDirectory()) {
      out = path.join(out, filename);
    }
    fs.writeFileSync(out, buf);
    return out;
  }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

const _MIME_MAP = {
  '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg',
  '.gif': 'image/gif', '.webp': 'image/webp', '.svg': 'image/svg+xml',
  '.pdf': 'application/pdf', '.mp3': 'audio/mpeg', '.wav': 'audio/wav',
  '.mp4': 'video/mp4', '.json': 'application/json',
  '.txt': 'text/plain', '.md': 'text/markdown',
};

function _mimeFromFilename(filename) {
  const path = require('path');
  const ext = path.extname(filename).toLowerCase();
  return _MIME_MAP[ext] || 'application/octet-stream';
}

function _decodeFileEnvelope(msg) {
  try {
    const env = JSON.parse(msg.data);
    if (env.encoding !== 'base64' || !env.data) return null;
    const buf = Buffer.from(env.data, 'base64');
    return { buf, filename: env.filename || 'data.bin', mime: msg.mime };
  } catch (_) {
    return null;
  }
}

module.exports = { Bus, Message, BusError };
