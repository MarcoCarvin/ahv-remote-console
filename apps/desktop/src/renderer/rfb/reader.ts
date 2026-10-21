/**
 * Streaming binary reader over an arbitrary supply of ArrayBuffers. Used by
 * the RFB client to consume the framebuffer-update message format which is
 * not chunk-aligned with WebSocket frames.
 *
 * The reader buffers incoming chunks and exposes synchronous "have at least
 * N bytes?" checks. Callers structured as state machines re-enter on every
 * onmessage and yield when more data is required.
 */
export class StreamReader {
  private chunks: Uint8Array[] = [];
  private len = 0;
  private offset = 0;

  push(chunk: ArrayBuffer | Uint8Array): void {
    const view = chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk);
    this.chunks.push(view);
    this.len += view.length;
  }

  available(): number {
    // `len` is decremented inside compact() on every consumed byte, so it
    // already represents "bytes still pending." Subtracting `offset` here
    // would double-count: `offset` is the cursor inside chunks[0], not an
    // independent measure of absolute consumption.
    return this.len;
  }

  /** Returns true if at least `n` bytes are pending. */
  has(n: number): boolean {
    return this.available() >= n;
  }

  /** Consolidate so the next `n` bytes are contiguous in `this.chunks[0]`. */
  private compact(n: number): Uint8Array {
    if (!Number.isInteger(n) || n < 0) {
      // Catch the desync class of bugs ("read N bytes" where N was computed
      // from misaligned wire data and came out negative) with a clear error
      // instead of the cryptic "Invalid typed array length: -N" thrown later
      // by `new Uint8Array(n)`.
      throw new Error(`StreamReader.compact: invalid length ${n}`);
    }
    if (!this.has(n)) throw new Error('StreamReader.compact: insufficient bytes');
    // If the first chunk has enough after the offset, slice directly.
    const first = this.chunks[0]!;
    if (first.length - this.offset >= n) {
      const out = first.subarray(this.offset, this.offset + n);
      this.offset += n;
      this.len -= n;
      // Drop the leading chunk when fully consumed.
      if (this.offset === first.length) {
        this.chunks.shift();
        this.offset = 0;
      }
      return out;
    }
    // Otherwise, copy into a temporary buffer.
    const out = new Uint8Array(n);
    let copied = 0;
    while (copied < n) {
      const head = this.chunks[0]!;
      const take = Math.min(head.length - this.offset, n - copied);
      out.set(head.subarray(this.offset, this.offset + take), copied);
      copied += take;
      this.offset += take;
      this.len -= take;
      if (this.offset === head.length) {
        this.chunks.shift();
        this.offset = 0;
      }
    }
    return out;
  }

  readU8(): number {
    const b = this.compact(1);
    return b[0]!;
  }

  readU16(): number {
    const b = this.compact(2);
    return (b[0]! << 8) | b[1]!;
  }

  readI16(): number {
    const v = this.readU16();
    return v >= 0x8000 ? v - 0x10000 : v;
  }

  readU32(): number {
    const b = this.compact(4);
    return (b[0]! * 0x1000000 + (b[1]! << 16) + (b[2]! << 8) + b[3]!) >>> 0;
  }

  readI32(): number {
    const v = this.readU32();
    return v > 0x7fffffff ? v - 0x100000000 : v;
  }

  readBytes(n: number): Uint8Array {
    // Always return a copy so callers can hold onto it without aliasing.
    const slice = this.compact(n);
    const out = new Uint8Array(n);
    out.set(slice);
    return out;
  }

  /**
   * Zero-copy variant of readBytes for hot decode paths. When the read fits
   * the head chunk the result is a subarray view straight into the receive
   * buffer (no allocation, no copy); a read spanning chunks still falls back
   * to the same copying reassembly as readBytes.
   *
   * Contract: the view is valid only until the next reader call; do not
   * retain it; while live it pins the backing WebSocket ArrayBuffer.
   */
  readBytesView(n: number): Uint8Array {
    return this.compact(n);
  }

  /** Peek the next byte without consuming it. */
  peekU8(): number {
    if (!this.has(1)) throw new Error('StreamReader.peekU8: empty');
    const first = this.chunks[0]!;
    return first[this.offset]!;
  }

  /**
   * Non-destructive peek of `n` bytes starting `skip` bytes ahead of the
   * current offset. Used by encoders that must look ahead to determine the
   * payload size of a partially-arrived message before they consume any of
   * it. Returns a fresh copy so callers cannot accidentally mutate the
   * underlying buffers.
   */
  peekAt(skip: number, n: number): Uint8Array {
    if (!this.has(skip + n)) throw new Error('StreamReader.peekAt: insufficient bytes');
    const out = new Uint8Array(n);
    let chunkIdx = 0;
    let chunkOff = this.offset + skip;
    while (chunkIdx < this.chunks.length && chunkOff >= this.chunks[chunkIdx]!.length) {
      chunkOff -= this.chunks[chunkIdx]!.length;
      chunkIdx++;
    }
    let copied = 0;
    while (copied < n) {
      const chunk = this.chunks[chunkIdx]!;
      const take = Math.min(chunk.length - chunkOff, n - copied);
      out.set(chunk.subarray(chunkOff, chunkOff + take), copied);
      copied += take;
      chunkIdx++;
      chunkOff = 0;
    }
    return out;
  }
}
