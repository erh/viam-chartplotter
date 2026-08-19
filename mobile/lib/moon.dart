import 'dart:math' as math;

/// Moonrise/moonset calculation — ported verbatim from src/lib/moon.ts
/// (tests translated alongside), itself ported from SunCalc
/// (https://github.com/mourner/suncalc, BSD-2-Clause, (c) Vladimir Agafonkin).
/// Open-Meteo's forecast API has no moon data, so unlike sunrise/sunset these
/// are computed client-side.

const double _rad = math.pi / 180;
const double _dayMs = 1000 * 60 * 60 * 24;
const double _j1970 = 2440588;
const double _j2000 = 2451545;

// obliquity of the Earth
const double _e = _rad * 23.4397;

double _toDays(DateTime date) =>
    date.millisecondsSinceEpoch / _dayMs - 0.5 + _j1970 - _j2000;

double _rightAscension(double l, double b) => math.atan2(
    math.sin(l) * math.cos(_e) - math.tan(b) * math.sin(_e), math.cos(l));

double _declination(double l, double b) => math.asin(
    math.sin(b) * math.cos(_e) + math.cos(b) * math.sin(_e) * math.sin(l));

double _altitude(double bigH, double phi, double dec) => math.asin(
    math.sin(phi) * math.sin(dec) +
        math.cos(phi) * math.cos(dec) * math.cos(bigH));

double _siderealTime(double d, double lw) =>
    _rad * (280.16 + 360.9856235 * d) - lw;

double _astroRefraction(double h) {
  if (h < 0) h = 0; // formula only works for positive altitudes
  return 0.0002967 / math.tan(h + 0.00312536 / (h + 0.08901179));
}

// geocentric ecliptic coordinates of the moon
({double ra, double dec}) _moonCoords(double d) {
  final bigL = _rad * (218.316 + 13.176396 * d); // ecliptic longitude
  final bigM = _rad * (134.963 + 13.064993 * d); // mean anomaly
  final bigF = _rad * (93.272 + 13.22935 * d); // mean distance
  final l = bigL + _rad * 6.289 * math.sin(bigM); // longitude
  final b = _rad * 5.128 * math.sin(bigF); // latitude
  return (ra: _rightAscension(l, b), dec: _declination(l, b));
}

double _moonAltitude(DateTime date, double lat, double lng) {
  final lw = _rad * -lng;
  final phi = _rad * lat;
  final d = _toDays(date);
  final c = _moonCoords(d);
  final bigH = _siderealTime(d, lw) - c.ra;
  final h = _altitude(bigH, phi, c.dec);
  return h + _astroRefraction(h);
}

// JS Date truncates fractional milliseconds, so floor keeps the two ports
// bit-identical at second precision.
DateTime _hoursLater(DateTime date, double h) => DateTime.fromMillisecondsSinceEpoch(
    date.millisecondsSinceEpoch + (h * _dayMs / 24).floor(),
    isUtc: date.isUtc);

class MoonTimes {
  const MoonTimes({
    required this.rise,
    required this.set,
    required this.alwaysUp,
    required this.alwaysDown,
  });

  final DateTime? rise;
  final DateTime? set;
  // At extreme latitudes the moon can stay above/below the horizon all day.
  final bool alwaysUp;
  final bool alwaysDown;
}

/// Rise/set times for the calendar day containing [date] (local midnight to
/// midnight, or UTC midnight when [inUTC] is set). Either can be null on a
/// normal day — the moon rises ~50 min later each day, so some days skip one.
MoonTimes getMoonTimes(DateTime date, double lat, double lng,
    {bool inUTC = false}) {
  final DateTime t;
  if (inUTC) {
    final u = date.toUtc();
    t = DateTime.utc(u.year, u.month, u.day);
  } else {
    final l = date.toLocal();
    t = DateTime(l.year, l.month, l.day);
  }

  const hc = 0.133 * _rad;
  var h0 = _moonAltitude(t, lat, lng) - hc;
  var rise = 0.0;
  var set = 0.0;
  var ye = 0.0;

  // go in 2-hour chunks, each time seeing if a 3-point quadratic curve
  // crosses zero (which means rise or set)
  for (var i = 1; i <= 24; i += 2) {
    final h1 = _moonAltitude(_hoursLater(t, i.toDouble()), lat, lng) - hc;
    final h2 = _moonAltitude(_hoursLater(t, i + 1.0), lat, lng) - hc;

    final a = (h0 + h2) / 2 - h1;
    final b = (h2 - h0) / 2;
    final xe = -b / (2 * a);
    ye = (a * xe + b) * xe + h1;
    final d = b * b - 4 * a * h1;
    var roots = 0;
    var x1 = 0.0;
    var x2 = 0.0;

    if (d >= 0) {
      final dx = math.sqrt(d) / (a.abs() * 2);
      x1 = xe - dx;
      x2 = xe + dx;
      if (x1.abs() <= 1) roots++;
      if (x2.abs() <= 1) roots++;
      if (x1 < -1) x1 = x2;
    }

    if (roots == 1) {
      if (h0 < 0) {
        rise = i + x1;
      } else {
        set = i + x1;
      }
    } else if (roots == 2) {
      rise = i + (ye < 0 ? x2 : x1);
      set = i + (ye < 0 ? x1 : x2);
    }

    if (rise != 0 && set != 0) break;
    h0 = h2;
  }

  return MoonTimes(
    rise: rise != 0 ? _hoursLater(t, rise) : null,
    set: set != 0 ? _hoursLater(t, set) : null,
    alwaysUp: rise == 0 && set == 0 && ye > 0,
    alwaysDown: rise == 0 && set == 0 && ye <= 0,
  );
}
