// Sunrise/sunset calculation derived from SunCalc (BSD-2-Clause,
// https://github.com/mourner/suncalc) — the same base as ./moon.ts. Pure
// math, no API: night-mode auto-switching needs sun times even before the
// weather fetch has run (and open-meteo only hands back formatted strings).

const rad = Math.PI / 180;
const dayMs = 1000 * 60 * 60 * 24;
const J1970 = 2440588;
const J2000 = 2451545;
const J0 = 0.0009;
const e = rad * 23.4397; // obliquity of the Earth

function toJulian(date: Date): number {
  return date.valueOf() / dayMs - 0.5 + J1970;
}
function fromJulian(j: number): Date {
  return new Date((j + 0.5 - J1970) * dayMs);
}
function toDays(date: Date): number {
  return toJulian(date) - J2000;
}

function solarMeanAnomaly(d: number): number {
  return rad * (357.5291 + 0.98560028 * d);
}
function eclipticLongitude(M: number): number {
  const C = rad * (1.9148 * Math.sin(M) + 0.02 * Math.sin(2 * M) + 0.0003 * Math.sin(3 * M));
  const P = rad * 102.9372; // perihelion of the Earth
  return M + C + P + Math.PI;
}
function declination(l: number, b: number): number {
  return Math.asin(Math.sin(b) * Math.cos(e) + Math.cos(b) * Math.sin(e) * Math.sin(l));
}

function julianCycle(d: number, lw: number): number {
  return Math.round(d - J0 - lw / (2 * Math.PI));
}
function approxTransit(Ht: number, lw: number, n: number): number {
  return J0 + (Ht + lw) / (2 * Math.PI) + n;
}
function solarTransitJ(ds: number, M: number, L: number): number {
  return J2000 + ds + 0.0053 * Math.sin(M) - 0.0069 * Math.sin(2 * L);
}
function hourAngle(h: number, phi: number, d: number): number {
  return Math.acos(
    (Math.sin(h) - Math.sin(phi) * Math.sin(d)) / (Math.cos(phi) * Math.cos(d))
  );
}

export interface SunTimes {
  sunrise: Date | null; // null at polar latitudes with no rise/set that day
  sunset: Date | null;
}

/** Sunrise/sunset (top of the sun at the horizon, -0.833°) for the calendar
 *  day containing `date`, at lat/lng in degrees. */
export function getSunTimes(date: Date, lat: number, lng: number): SunTimes {
  const lw = rad * -lng;
  const phi = rad * lat;
  const d = toDays(date);
  const n = julianCycle(d, lw);
  const ds = approxTransit(0, lw, n);
  const M = solarMeanAnomaly(ds);
  const L = eclipticLongitude(M);
  const dec = declination(L, 0);
  const Jnoon = solarTransitJ(ds, M, L);

  const h0 = -0.833 * rad; // sunrise/sunset altitude
  const w = hourAngle(h0, phi, dec);
  if (!Number.isFinite(w)) return { sunrise: null, sunset: null }; // polar day/night
  const Jset = solarTransitJ(approxTransit(w, lw, n), M, L);
  const Jrise = Jnoon - (Jset - Jnoon);
  return { sunrise: fromJulian(Jrise), sunset: fromJulian(Jset) };
}

/** Whether night mode should be on at `now`: from 15 min after sunset until
 *  15 min before the NEXT sunrise. Polar day → never; polar night → always.
 *  `marginMin` is exposed for tests. */
export function isNightAt(now: Date, lat: number, lng: number, marginMin = 15): boolean {
  const { sunrise, sunset } = getSunTimes(now, lat, lng);
  if (sunrise == null || sunset == null) {
    // No rise/set today: decide by the sun's position — reuse declination
    // via a noon check: polar night when even solar noon is below horizon.
    const d = toDays(now);
    const M = solarMeanAnomaly(d);
    const dec = declination(eclipticLongitude(M), 0);
    const noonAlt = Math.PI / 2 - Math.abs(rad * lat - dec);
    return noonAlt < 0;
  }
  const margin = marginMin * 60 * 1000;
  const t = now.valueOf();
  if (t >= sunset.valueOf() + margin) return true; // evening onward
  if (t < sunrise.valueOf() - margin) return true; // small hours
  return false;
}
