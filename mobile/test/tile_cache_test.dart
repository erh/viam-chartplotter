import 'dart:io';
import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:viam_chartplotter_mobile/map/tile_cache.dart';

// L1 — tile disk cache: keying, freshness, LRU eviction, offline fallback.

Uint8List bytesOf(int n, [int fill = 7]) =>
    Uint8List.fromList(List.filled(n, fill));

void main() {
  late Directory dir;
  late TileDiskCache cache;

  setUp(() async {
    dir = await Directory.systemTemp.createTemp('tile-cache-test');
    cache = TileDiskCache(dir: dir, maxBytes: 10 * 1024);
  });

  tearDown(() async {
    try {
      await dir.delete(recursive: true);
    } catch (_) {}
  });

  test('round-trips a tile and reports it fresh', () async {
    await cache.put('http://x/tile/1?v=a&sd=6', bytesOf(100));
    final got = await cache.get('http://x/tile/1?v=a&sd=6');
    expect(got, isNotNull);
    expect(got!.fresh, isTrue);
    expect(got.bytes, bytesOf(100));
  });

  test('keys on the FULL url — v= and sd= changes are different entries',
      () async {
    await cache.put('http://x/t?style=wms&v=a&sd=6', bytesOf(10, 1));
    await cache.put('http://x/t?style=wms&v=a&sd=8', bytesOf(10, 2));
    await cache.put('http://x/t?style=wms&v=b&sd=6', bytesOf(10, 3));
    expect((await cache.get('http://x/t?style=wms&v=a&sd=6'))!.bytes,
        bytesOf(10, 1));
    expect((await cache.get('http://x/t?style=wms&v=a&sd=8'))!.bytes,
        bytesOf(10, 2));
    expect((await cache.get('http://x/t?style=wms&v=b&sd=6'))!.bytes,
        bytesOf(10, 3));
    expect(await cache.get('http://x/t?style=wms&v=c&sd=6'), isNull);
  });

  test('a tile older than the ttl reads back stale, not fresh', () async {
    final shortTtl =
        TileDiskCache(dir: dir, ttl: const Duration(minutes: 5));
    await shortTtl.put('http://x/old', bytesOf(10));
    // Backdate the file past the ttl.
    final f = dir.listSync().whereType<File>().single;
    await f.setLastModified(
        DateTime.now().subtract(const Duration(minutes: 10)));
    final got = await shortTtl.get('http://x/old');
    expect(got, isNotNull);
    expect(got!.fresh, isFalse);
  });

  test('usage and clear', () async {
    await cache.put('a', bytesOf(1000));
    await cache.put('b', bytesOf(2000));
    expect(await cache.usageBytes(), 3000);
    await cache.clear();
    expect(await cache.usageBytes(), 0);
    expect(await cache.get('a'), isNull);
  });

  test('evicts oldest-first and stays under the bound', () async {
    // Fill with an unbounded cache so nothing evicts mid-setup, stamping
    // strictly increasing mtimes (filesystem clocks are too coarse to order
    // sub-second writes).
    final filler = TileDiskCache(dir: dir, maxBytes: 1 << 30);
    final seen = <String>{};
    for (var i = 0; i < 6; i++) {
      await filler.put('tile-$i', bytesOf(3 * 1024));
      final f = dir
          .listSync()
          .whereType<File>()
          .firstWhere((f) => !seen.contains(f.path));
      seen.add(f.path);
      await f.setLastModified(DateTime(2026, 1, 1).add(Duration(minutes: i)));
    }
    // One more put through a 10 KiB-bounded cache triggers eviction:
    // 21 KiB present → oldest go first until under the bound.
    final bounded = TileDiskCache(dir: dir, maxBytes: 10 * 1024);
    await bounded.put('tile-final', bytesOf(3 * 1024));
    await Future<void>.delayed(
        const Duration(milliseconds: 300)); // unawaited eviction settles
    final usage =
        await TileDiskCache(dir: dir).usageBytes(); // fresh scan, no memo
    expect(usage, lessThanOrEqualTo(10 * 1024));
    expect(await bounded.get('tile-0'), isNull); // oldest went first
    expect(await bounded.get('tile-final'), isNotNull); // newest survived
  });

  group('fetchTileCached', () {
    test('fresh hit issues no network request', () async {
      await cache.put('http://x/hit', bytesOf(10));
      var calls = 0;
      final client = MockClient((req) async {
        calls++;
        return http.Response.bytes(bytesOf(5), 200);
      });
      final got =
          await fetchTileCached('http://x/hit', const {}, cache: cache, client: client);
      expect(got, bytesOf(10));
      expect(calls, 0);
      expect(cache.hits, 1);
    });

    test('miss fetches, stores, and counts bytes', () async {
      final client =
          MockClient((req) async => http.Response.bytes(bytesOf(42), 200));
      final got = await fetchTileCached('http://x/miss', const {},
          cache: cache, client: client);
      expect(got, bytesOf(42));
      expect(cache.misses, 1);
      expect(cache.netBytes, 42);
      expect((await cache.get('http://x/miss'))!.bytes, bytesOf(42));
    });

    test('offline serves the stale copy instead of failing', () async {
      final stale = TileDiskCache(dir: dir, ttl: Duration.zero);
      await stale.put('http://x/stale', bytesOf(10));
      final client = MockClient((req) async => throw const SocketException('down'));
      final got = await fetchTileCached('http://x/stale', const {},
          cache: stale, client: client);
      expect(got, bytesOf(10));
    });

    test('offline with nothing cached rethrows', () async {
      final client = MockClient((req) async => throw const SocketException('down'));
      expect(
          () => fetchTileCached('http://x/none', const {},
              cache: cache, client: client),
          throwsA(isA<SocketException>()));
    });

    test('an HTTP error falls back to the stale copy too', () async {
      final stale = TileDiskCache(dir: dir, ttl: Duration.zero);
      await stale.put('http://x/500', bytesOf(10));
      final client = MockClient((req) async => http.Response('boom', 500));
      final got = await fetchTileCached('http://x/500', const {},
          cache: stale, client: client);
      expect(got, bytesOf(10));
    });
  });
}
