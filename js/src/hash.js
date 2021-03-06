// @flow

// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

import {alloc, compare, sha512} from './bytes.js';
import {encode, decode} from './base32';

export const byteLength = 20;
export const stringLength = 32;
const pattern = /^[0-9a-v]{32}$/;

export default class Hash {
  _digest: Uint8Array;

  /**
   * The Hash instance does not copy the `digest` so if the `digest` is part of a large ArrayBuffer
   * the caller might want to make a copy first to prevent that ArrayBuffer from being retained.
   */
  constructor(digest: Uint8Array) {
    this._digest = digest;
  }

  get digest(): Uint8Array {
    return this._digest;
  }

  isEmpty(): boolean {
    return this.equals(emptyHash);
  }

  equals(other: Hash): boolean {
    return this.compare(other) === 0;
  }

  compare(other: Hash): number {
    return compare(this._digest, other._digest);
  }

  toString(): string {
    return encode(this._digest);
  }

  static parse(s: string): ?Hash {
    if (pattern.test(s)) {
      return new Hash(decode(s));
    }
    return null;
  }

  static fromData(data: Uint8Array): Hash {
    return new Hash(sha512(data));
  }
}

export const emptyHash = new Hash(alloc(byteLength));
