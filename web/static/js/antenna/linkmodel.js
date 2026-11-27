// antenna/linkmodel.js - small client-side 1090 MHz link model for the Antenna Lab "what-if" calculator.
//
// The server's link budget (/api/antenna/linkbudget, /api/antenna/budget) only describes the antennas that
// are configured. To let the user try other antenna heights, gains, cable losses and preamplifiers without
// saving anything, the same textbook maths runs here:
//
//   radio horizon      d = sqrt(2 k R h_antenna) + sqrt(2 k R h_aircraft)          (smooth earth, k = 4/3)
//   free-space loss    FSPL = 32.45 + 20 log10(f_MHz) + 20 log10(d_km)
//   system noise       T_sys = T_antenna + T0 (L F_chain - 1),  F_chain = F_lna + (F_rx - 1) / G_lna
//   sensitivity        k T_sys B + required SNR      (referred to the antenna terminals)
//   margin             P_tx + G_rx - FSPL - gas loss - sensitivity
//
// Defaults (transmit power, bandwidth, required SNR, receiver noise figure, gas loss) are calibrated
// from a /api/antenna/budget response when one is available, so both models agree for the configured antenna.

const R_EARTH_KM = 6371.0088;
const FT_TO_M = 0.3048;
const T0 = 290;
const BOLTZMANN_DBM_HZ = -198.599; // 10 log10(1.380649e-23 W/K/Hz) + 30

const db = (x) => 10 * Math.log10(x);
const lin = (dB) => 10 ** (dB / 10);
const num = (v, fallback) => (typeof v === 'number' && Number.isFinite(v) ? v : fallback);

/** Model constants; override any of them through calibrate() or by hand. */
export const DEFAULTS = Object.freeze({
  freqMHz: 1090,
  k: 4 / 3,                // effective earth radius factor
  txPowerDbm: 53.98,       // 250 W transponder
  antennaTempK: 150,       // sky + ground noise seen by the antenna
  rxNoiseFigDb: 6,         // receiver noise figure
  bandwidthHz: 2e6,
  requiredSnrDb: 10,
  gasDbPerKm: 0.0025,      // oxygen + water vapour at 1090 MHz, low elevation path
});

/**
 * Derive the model constants from a /api/antenna/budget reply (and optionally the antenna it was computed
 * for), so that this model reproduces the server's numbers. Unknown / odd values keep their defaults.
 * @param {object} reply server.budgetReply
 * @param {object} [antenna] model.Antenna the reply belongs to
 */
export function calibrate(reply, antenna) {
  const c = { ...DEFAULTS };
  const b = reply && reply.budget;
  if (!b) return c;
  c.txPowerDbm = num(b.txPowerDbm, c.txPowerDbm);
  c.requiredSnrDb = num(b.requiredSnrDb, c.requiredSnrDb);
  if (num(b.gasLossDb, -1) >= 0 && num(b.slantKm, 0) > 1) {
    const rate = b.gasLossDb / b.slantKm;
    if (rate >= 0 && rate < 0.05) c.gasDbPerKm = rate;
  }
  // bandwidth from noise floor = k T B
  if (num(b.systemTempK, 0) > 0 && typeof b.noiseFloorDbm === 'number') {
    const bw = 10 ** ((b.noiseFloorDbm - BOLTZMANN_DBM_HZ - db(b.systemTempK)) / 10);
    if (bw > 1e5 && bw < 2e7) c.bandwidthHz = bw;
  }
  // antenna temperature: only identifiable for a chain without preamplifier
  const lna = antenna ? num(antenna.lnaGainDb, 0) : num(b.lnaGainDb, 0);
  if (lna <= 0 && num(b.systemTempK, 0) > 0) {
    const loss = num(b.frontEndLossDb, 0);
    const ta = b.systemTempK - T0 * (lin(loss + c.rxNoiseFigDb) - 1);
    if (ta >= 0 && ta < 2000) c.antennaTempK = ta;
  }
  return c;
}

/** Smooth-earth radio horizon in km between an antenna (m AMSL) and an aircraft (ft AMSL). */
export function horizonKm(antennaAmslM, altFt, k = DEFAULTS.k) {
  const ha = Math.max(0, num(antennaAmslM, 0)) / 1000;
  const hb = Math.max(0, num(altFt, 0)) * FT_TO_M / 1000;
  const r = k * R_EARTH_KM;
  return Math.sqrt(2 * r * ha) + Math.sqrt(2 * r * hb);
}

/**
 * System noise of a receive chain, referred to the antenna terminals.
 * chain: { cableLossDb, filterLossDb, lnaGainDb, lnaNoiseFigDb }
 * @returns {{ lossDb, noiseFigDb, tempK, noiseFloorDbm, sensitivityDbm }}
 */
export function systemNoise(chain, c = DEFAULTS) {
  const lossDb = Math.max(0, num(chain.cableLossDb, 0)) + Math.max(0, num(chain.filterLossDb, 0));
  const gLna = num(chain.lnaGainDb, 0);
  let fChain = lin(c.rxNoiseFigDb);
  if (gLna > 0) fChain = lin(Math.max(0, num(chain.lnaNoiseFigDb, 1))) + (lin(c.rxNoiseFigDb) - 1) / lin(gLna);
  const f = lin(lossDb) * fChain;
  const tempK = c.antennaTempK + T0 * (f - 1);
  const noiseFloorDbm = BOLTZMANN_DBM_HZ + db(tempK) + db(c.bandwidthHz);
  return { lossDb, noiseFigDb: db(f), tempK, noiseFloorDbm, sensitivityDbm: noiseFloorDbm + c.requiredSnrDb };
}

/** Link margin in dB at ground distance `km` for an aircraft at altFt. */
export function marginDb(km, altFt, setup, noise, c = DEFAULTS) {
  const dh = (num(altFt, 0) * FT_TO_M - num(setup.antennaAmslM, 0)) / 1000;
  const slant = Math.max(0.05, Math.hypot(km, dh));
  const fspl = 32.45 + 20 * Math.log10(c.freqMHz) + 20 * Math.log10(slant);
  const tx = num(setup.txPowerDbm, c.txPowerDbm);
  return tx + num(setup.gainDbi, 0) - fspl - c.gasDbPerKm * slant - noise.sensitivityDbm;
}

/**
 * Predicted maximum range for one altitude.
 * setup: { antennaAmslM, gainDbi, cableLossDb, filterLossDb, lnaGainDb, lnaNoiseFigDb, txPowerDbm }
 * @returns {{ rangeKm, horizonKm, limit: 'horizon'|'power', marginDb (at the returned range), marginAtHorizonDb, noise }}
 */
export function predict(setup, altFt, c = DEFAULTS) {
  const noise = systemNoise(setup, c);
  const hz = horizonKm(setup.antennaAmslM, altFt, c.k);
  const atHorizon = marginDb(hz, altFt, setup, noise, c);
  if (atHorizon >= 0) return { rangeKm: hz, horizonKm: hz, limit: 'horizon', marginDb: atHorizon, marginAtHorizonDb: atHorizon, noise };
  // power limited: the margin falls monotonically with distance beyond the overhead point -> bisection
  let lo = 0;
  let hi = hz;
  if (marginDb(0, altFt, setup, noise, c) < 0) return { rangeKm: 0, horizonKm: hz, limit: 'power', marginDb: marginDb(0, altFt, setup, noise, c), marginAtHorizonDb: atHorizon, noise };
  for (let i = 0; i < 48; i++) {
    const mid = (lo + hi) / 2;
    if (marginDb(mid, altFt, setup, noise, c) >= 0) lo = mid; else hi = mid;
  }
  return { rangeKm: lo, horizonKm: hz, limit: 'power', marginDb: 0, marginAtHorizonDb: atHorizon, noise };
}

export default { DEFAULTS, calibrate, horizonKm, systemNoise, marginDb, predict };
