// Moonrise/moonset calculation, ported from SunCalc
// (https://github.com/mourner/suncalc, BSD-2-Clause, (c) Vladimir Agafonkin).
// Open-Meteo's forecast API has no moon data, so unlike sunrise/sunset these
// are computed client-side.

const rad = Math.PI / 180;
const dayMs = 1000 * 60 * 60 * 24;
const J1970 = 2440588;
const J2000 = 2451545;

// obliquity of the Earth
const e = rad * 23.4397;

function toDays(date: Date): number {
  return date.valueOf() / dayMs - 0.5 + J1970 - J2000;
}

function rightAscension(l: number, b: number): number {
  return Math.atan2(Math.sin(l) * Math.cos(e) - Math.tan(b) * Math.sin(e), Math.cos(l));
}

function declination(l: number, b: number): number {
  return Math.asin(Math.sin(b) * Math.cos(e) + Math.cos(b) * Math.sin(e) * Math.sin(l));
}

function altitude(H: number, phi: number, dec: number): number {
  return Math.asin(Math.sin(phi) * Math.sin(dec) + Math.cos(phi) * Math.cos(dec) * Math.cos(H));
}

function siderealTime(d: number, lw: number): number {
  return rad * (280.16 + 360.9856235 * d) - lw;
}

function astroRefraction(h: number): number {
  if (h < 0) h = 0; // formula only works for positive altitudes
  return 0.0002967 / Math.tan(h + 0.00312536 / (h + 0.08901179));
}

// geocentric ecliptic coordinates of the moon
function moonCoords(d: number): { ra: number; dec: number } {
  const L = rad * (218.316 + 13.176396 * d); // ecliptic longitude
  const M = rad * (134.963 + 13.064993 * d); // mean anomaly
  const F = rad * (93.272 + 13.22935 * d); // mean distance
  const l = L + rad * 6.289 * Math.sin(M); // longitude
  const b = rad * 5.128 * Math.sin(F); // latitude
  return { ra: rightAscension(l, b), dec: declination(l, b) };
}

function moonAltitude(date: Date, lat: number, lng: number): number {
  const lw = rad * -lng;
  const phi = rad * lat;
  const d = toDays(date);
  const c = moonCoords(d);
  const H = siderealTime(d, lw) - c.ra;
  const h = altitude(H, phi, c.dec);
  return h + astroRefraction(h);
}

function hoursLater(date: Date, h: number): Date {
  return new Date(date.valueOf() + (h * dayMs) / 24);
}

export interface MoonTimes {
  rise: Date | null;
  set: Date | null;
  // At extreme latitudes the moon can stay above/below the horizon all day.
  alwaysUp: boolean;
  alwaysDown: boolean;
}

// Rise/set times for the calendar day containing `date` (local midnight to
// midnight, or UTC midnight when inUTC is set). Either can be null on a
// normal day — the moon rises ~50 min later each day, so some days skip one.
export function getMoonTimes(date: Date, lat: number, lng: number, inUTC = false): MoonTimes {
  const t = new Date(date);
  if (inUTC) t.setUTCHours(0, 0, 0, 0);
  else t.setHours(0, 0, 0, 0);

  const hc = 0.133 * rad;
  let h0 = moonAltitude(t, lat, lng) - hc;
  let rise = 0;
  let set = 0;
  let ye = 0;

  // go in 2-hour chunks, each time seeing if a 3-point quadratic curve
  // crosses zero (which means rise or set)
  for (let i = 1; i <= 24; i += 2) {
    const h1 = moonAltitude(hoursLater(t, i), lat, lng) - hc;
    const h2 = moonAltitude(hoursLater(t, i + 1), lat, lng) - hc;

    const a = (h0 + h2) / 2 - h1;
    const b = (h2 - h0) / 2;
    const xe = -b / (2 * a);
    ye = (a * xe + b) * xe + h1;
    const d = b * b - 4 * a * h1;
    let roots = 0;
    let x1 = 0;
    let x2 = 0;

    if (d >= 0) {
      const dx = Math.sqrt(d) / (Math.abs(a) * 2);
      x1 = xe - dx;
      x2 = xe + dx;
      if (Math.abs(x1) <= 1) roots++;
      if (Math.abs(x2) <= 1) roots++;
      if (x1 < -1) x1 = x2;
    }

    if (roots === 1) {
      if (h0 < 0) rise = i + x1;
      else set = i + x1;
    } else if (roots === 2) {
      rise = i + (ye < 0 ? x2 : x1);
      set = i + (ye < 0 ? x1 : x2);
    }

    if (rise && set) break;
    h0 = h2;
  }

  return {
    rise: rise ? hoursLater(t, rise) : null,
    set: set ? hoursLater(t, set) : null,
    alwaysUp: !rise && !set && ye > 0,
    alwaysDown: !rise && !set && ye <= 0,
  };
}
