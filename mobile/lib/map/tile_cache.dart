import 'dart:async';
import 'dart:io';
import 'dart:ui' as ui;

import 'package:crypto/crypto.dart' show sha1;
import 'package:flutter/foundation.dart';
import 'package:flutter/painting.dart';
import 'package:http/http.dart' as http;
import 'package:path_provider/path_provider.dart';

/// Disk-backed tile cache (L1). Evaluated per the card: flutter_map_tile_caching
/// drags in an ObjectBox database (and a GPL/commercial licence);
/// flutter_map_cache's file store has no size-bounded LRU. A tile store is
/// simple enough to own outright: one file per tile keyed by the SHA-1 of the
/// FULL request URL — the query string carries `v=` (build) and `sd=` (safe
/// depth), so a version bump or draft change is a different key and can never
/// serve stale pixels (the A3 dependency).
///
/// Freshness: a tile younger than [ttl] is served straight from disk with no
/// network I/O (server-side chart renders are expensive and offshore data is
/// metered). Older tiles are refetched — but on ANY network failure the stale
/// copy is served instead, which is what keeps the chart alive offline.
///
/// Bounding: file mtime is the LRU clock (touched on every hit); when usage
/// passes [maxBytes] the oldest files go first, down to 90% so eviction runs
/// in bursts rather than on every put.
class TileDiskCache {
  TileDiskCache({required this.dir, this.maxBytes = defaultMaxBytes, this.ttl = defaultTtl});

  /// ~250 MB: roomy enough for a season of local water at z7–16, small
  /// enough to be invisible on a modern phone.
  static const int defaultMaxBytes = 250 * 1024 * 1024;
  static const Duration defaultTtl = Duration(days: 7);

  final Directory dir;
  final int maxBytes;
  final Duration ttl;

  // ---- session counters (surfaced by L3's data-budget screen) ----------
  int hits = 0; // served from disk (fresh or stale-on-error)
  int misses = 0; // had to go to the network
  int netBytes = 0; // bytes actually downloaded this session

  int? _usage; // running total; scanned once, then maintained
  bool _evicting = false;

  /// The app-wide instance, once [init] has run. Null in tests/headless —
  /// callers fall back to uncached network fetches.
  static TileDiskCache? instance;

  static Future<void> init() async {
    try {
      final base = await getApplicationCacheDirectory();
      final d = Directory('${base.path}/tiles');
      await d.create(recursive: true);
      instance = TileDiskCache(dir: d);
    } catch (e) {
      debugPrint('tile cache unavailable: $e');
    }
  }

  File _fileFor(String url) =>
      File('${dir.path}/${sha1.convert(url.codeUnits)}.png');

  /// The cached tile and whether it's still within [ttl]. Touches the mtime
  /// so recently-viewed water survives eviction (LRU).
  Future<({Uint8List bytes, bool fresh})?> get(String url) async {
    final f = _fileFor(url);
    try {
      final stat = await f.stat();
      if (stat.type == FileSystemEntityType.notFound) return null;
      final bytes = await f.readAsBytes();
      final fresh = DateTime.now().difference(stat.modified) <= ttl;
      // A hit refreshes the LRU clock but must NOT extend freshness — a
      // separate marker would cost a second file, so accept that touching
      // resets both; the v= cache-buster still catches real chart updates.
      if (fresh) unawaited(f.setLastModified(DateTime.now()).catchError((_) {}));
      return (bytes: bytes, fresh: fresh);
    } catch (_) {
      return null;
    }
  }

  Future<void> put(String url, Uint8List bytes) async {
    try {
      final f = _fileFor(url);
      final existed = await f.exists();
      final oldLen = existed ? await f.length() : 0;
      await f.writeAsBytes(bytes, flush: false);
      if (_usage != null) _usage = _usage! - oldLen + bytes.length;
      unawaited(_evictIfNeeded());
    } catch (_) {
      // A full disk must never take the chart down with it.
    }
  }

  Future<int> usageBytes() async {
    final cached = _usage;
    if (cached != null) return cached;
    var total = 0;
    try {
      await for (final e in dir.list()) {
        if (e is File) total += await e.length();
      }
    } catch (_) {}
    _usage = total;
    return total;
  }

  Future<void> clear() async {
    try {
      await for (final e in dir.list()) {
        if (e is File) await e.delete();
      }
    } catch (_) {}
    _usage = 0;
  }

  /// Oldest-mtime-first eviction down to 90% of [maxBytes].
  Future<void> _evictIfNeeded() async {
    if (_evicting) return;
    if (await usageBytes() <= maxBytes) return;
    _evicting = true;
    try {
      final files = <({File f, DateTime m, int len})>[];
      await for (final e in dir.list()) {
        if (e is File) {
          final s = await e.stat();
          files.add((f: e, m: s.modified, len: s.size));
        }
      }
      files.sort((a, b) => a.m.compareTo(b.m));
      var usage = files.fold(0, (t, e) => t + e.len);
      final target = (maxBytes * 0.9).round();
      for (final e in files) {
        if (usage <= target) break;
        try {
          await e.f.delete();
          usage -= e.len;
        } catch (_) {}
      }
      _usage = usage;
    } finally {
      _evicting = false;
    }
  }
}

/// One shared client so tile fetches reuse connections instead of paying a
/// TLS handshake per tile — that matters on a slow cellular link.
final http.Client _sharedClient = http.Client();

/// Fetch [url] through the disk cache: fresh hit → no network at all;
/// otherwise network, falling back to any stale copy when the fetch fails
/// (offline at sea). Throws only when there is neither network nor cache.
Future<Uint8List> fetchTileCached(
  String url,
  Map<String, String> headers, {
  TileDiskCache? cache,
  http.Client? client, // injected by tests; default shared client
}) async {
  final c = cache ?? TileDiskCache.instance;
  final cached = c == null ? null : await c.get(url);
  if (cached != null && cached.fresh) {
    c!.hits++;
    return cached.bytes;
  }
  try {
    final resp = await (client ?? _sharedClient)
        .get(Uri.parse(url), headers: headers)
        .timeout(const Duration(seconds: 30));
    if (resp.statusCode != 200 || resp.bodyBytes.isEmpty) {
      throw HttpException('HTTP ${resp.statusCode}', uri: Uri.parse(url));
    }
    if (c != null) {
      c.misses++;
      c.netBytes += resp.bodyBytes.length;
      await c.put(url, resp.bodyBytes);
    }
    return resp.bodyBytes;
  } catch (_) {
    if (cached != null) {
      c!.hits++;
      return cached.bytes; // stale beats blank when offline
    }
    rethrow;
  }
}

/// ImageProvider over [fetchTileCached] — what the tile providers hand to
/// flutter_map in place of NetworkImage.
class CachedTileImage extends ImageProvider<CachedTileImage> {
  const CachedTileImage(this.url, this.headers);

  final String url;
  final Map<String, String> headers;

  @override
  Future<CachedTileImage> obtainKey(ImageConfiguration configuration) =>
      SynchronousFuture(this);

  @override
  ImageStreamCompleter loadImage(
      CachedTileImage key, ImageDecoderCallback decode) {
    return OneFrameImageStreamCompleter(_load(key, decode),
        informationCollector: () sync* {
      yield DiagnosticsProperty('URL', url);
    });
  }

  Future<ImageInfo> _load(
      CachedTileImage key, ImageDecoderCallback decode) async {
    final bytes = await fetchTileCached(key.url, key.headers);
    final buffer = await ui.ImmutableBuffer.fromUint8List(bytes);
    final codec = await decode(buffer);
    final frame = await codec.getNextFrame();
    return ImageInfo(image: frame.image);
  }

  @override
  bool operator ==(Object other) => other is CachedTileImage && other.url == url;

  @override
  int get hashCode => url.hashCode;
}
