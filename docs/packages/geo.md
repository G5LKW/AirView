# geo - API notes (from the package author)

All three packages are pure functions / immutable values, safe for concurrent use, stdlib + internal/model only. astro is standalone; radio imports geo and model.

## airview/internal/geo
Units: degrees (N/E positive), bearings true [0,360), km, heights in METRES (use FeetToMetres for API feet).

Constants: EarthRadiusKm=6371.0088, WGS84A/F/B, DefaultK=4/3, MetresPerFoot, FeetPerMetre, KmPerNM, KmPerMile.
Converters: Rad, Deg, FeetToMetres, MetresToFeet, KmToNM, NMToKm.
Normalisers: NormalizeDeg(d) [0,360); NormalizeLon(l) [-180,180); ClampLat; AngleDiff(a,b) signed (-180,180]; ValidLatLon(lat,lon) bool.

Great circle (sphere):
- HaversineKm(lat1,lon1,lat2,lon2)
- FastDistanceKm(...) equirectangular, coarse filtering only
- InitialBearing(...), FinalBearing(...)
- Destination(lat,lon,brg,distKm) (lat2,lon2)
- Midpoint(...), Intermediate(...,f)
- CrossTrackKm(latP,lonP,lat1,lon1,lat2,lon2) signed, + = right of track; AlongTrackKm(...)

WGS84:
- DistanceKm(...), DistanceBearing(...) (km,brg): Andoyer-Lambert, single pass, under 10 m / 0.05 deg error within 800 km. Recommended general-purpose distance.
- VincentyInverse(lat1,lon1,lat2,lon2) (distM, initBrg, finalBrg, ok); ok=false only near-antipodal (spherical fallback values returned)
- VincentyKm(...)
- VincentyDirect(lat,lon,brg,distM) (lat2,lon2,finalBrg)

Station geometry (effective earth; k=0 or NaN means 4/3, k<0 or >1000 means flat earth):
- EffectiveK(k)
- SlantRangeKm(groundKm, antennaM, altM)
- ElevationDeg(groundKm, antennaM, altM, k)
- HorizonKm(hM, k)
- MinVisibleAltitudeM(groundKm, antennaM, k): smooth-earth coverage floor
- AltitudeAtElevationM(groundKm, antennaM, elevDeg, k) (altM, ok)
- type Look {GroundKm, BearingDeg, SlantKm, ElevationDeg}
- type Station {Lat, Lon, AntennaAMSLM, K}; NewStation(lat, lon, antennaAMSLM, k) *Station (immutable; rebuild on config change)
- Station methods: DistanceBearing(lat,lon) (km,brg); DistanceKm; Look(lat,lon,altM) Look (altM NaN gives NaN slant/elev); LookFt(lat,lon,altFt); Destination(brg,distKm) (Vincenty, for range rings); MinVisibleAltitudeM(groundKm)

Hot-path usage:
  st := geo.NewStation(rx.Lat, rx.Lon, rx.GroundElevM+ant.HeightAGLM, 0)
  l := st.LookFt(lat, lon, altFt)   // about 90 ns, 0 allocs

Parsing:
- ParseLatLon(s) (lat, lon, format string, err). Accepts decimal (comma, space, semicolon, decimal commas), hemisphere letters prefix/suffix, DMS with or without symbols, degrees + decimal minutes, compact NMEA DDMM.m / DDMMSS, "lat:/lon:" labels, Maidenhead 4/6/8/10 (centre of square), full Plus Codes, geo: URIs, Google/Bing/OSM map URLs. Rejects out-of-range values and ambiguous input with a user-readable error.
- format is one of the Fmt* constants (FmtDecimal, FmtDecimalHemi, FmtDDM, FmtDMS, FmtCompactDM, FmtCompactDMS, FmtMaidenhead + " (N characters, centre of square)", FmtPlusCode, FmtGeoURI, FmtGoogleMapsPin, FmtGoogleMapsView, FmtMapURL, FmtOSMURL).
- ParseMaidenhead(loc) (lat,lon,err); ParsePlusCode(code) (lat,lon,err)

Formatting:
- FormatDecimal(lat,lon,decimals) e.g. "51.50740, -0.12780"
- FormatDMS(lat,lon,secDecimals) e.g. 51°30'26.6"N 0°07'40.1"W
- FormatDDM(lat,lon,minDecimals) e.g. "N51 30.444 W000 07.668"
- Maidenhead(lat,lon,chars 2..10) e.g. "IO91wm"
- Non-finite input gives "?" (DMS/DDM) or "" (Maidenhead).

DB binning conventions (tracker, store and analytics MUST use these). NaN gives -1; out-of-range clamps:
- Heat grid: HeatCellDeg=0.05, HeatLatCells=3600, HeatLonCells=7200. HeatCell(lat,lon) uint32 = (latIdx<<16)|lonIdx with latIdx=floor((lat+90)*20), lonIdx=floor((lon+180)*20). HeatCellFromIndices, HeatCellIndices(cell), HeatCellBounds(cell) (s,w,n,e), HeatCellCenter(cell).
- Bearing: BearingBinDeg=2, BearingBins=180, BearingBin(brg), BearingBinCenter(bin). SectorDeg=10, Sectors=36, SectorBin, SectorCenter (range_hour.sector_max).
- Altitude: AltBands=7. AltBand(altFt): 0 <1000, 1 <5000, 2 <10000, 3 <20000, 4 <30000, 5 <40000, 6 >=40000. AltBandBoundsFt(b), AltBandLabel(b) ("<1k".. ">40k"), AltBandTypicalFt(b) (500, 3000, 7500, 15000, 25000, 35000, 43000).
- Signal histogram: DistBinKm=10, DistBins=80, DistBin(km), DistBinCenterKm. RSSIBinDB=1, RSSIBins=60, RSSIBin(dBFS) (bin i = (-(i+1), -i]), RSSIBinCenter.
- Elevation: ElevBinDeg=0.5, ElevMinDeg=-2, ElevBins=184, ElevBin(elev), ElevBinCenter.
- Range histogram: RangeBinKm=5, RangeBins=120, RangeBin(km), RangeBinCenterKm.

## airview/internal/astro
NOAA/Meeus solar algorithm. Rings are [lon,lat] pairs.

Constants: SunriseAltitudeDeg=-0.833, CivilAltitudeDeg=-6, NauticalAltitudeDeg=-12, AstronomicalAltitudeDeg=-18, DefaultTerminatorStepDeg=2, WebMercatorMaxLat, SynodicMonthDays.

- JulianDay(t)
- Refraction(trueElevDeg) deg
- type SunPosition {AzimuthDeg, ElevationDeg (refraction-corrected), TrueElevationDeg, DeclinationDeg, HourAngleDeg, EqOfTimeMin, DistanceAU} (JSON tags azimuth, elevation, trueElevation, ...)
- SunAt(t, lat, lon) SunPosition
- SubsolarPoint(t) (lat, lon)
- type Phase int: Day, CivilTwilight, NauticalTwilight, AstronomicalTwilight, Night; String() gives "day" | "civil" | "nautical" | "astronomical" | "night"
- PhaseOfElevation(trueElev), PhaseAt(t,lat,lon), IsDay(t,lat,lon)
- type Events {SolarNoon, NoonElevationDeg, MidnightElevationDeg, Sunrise, Sunset, CivilDawn/Dusk, NauticalDawn/Dusk, AstroDawn/Dusk time.Time (zero = does not occur), DayLength, PolarDay, PolarNight}; Events.Ms() EventsMs (Unix ms, 0 = none, JSON-ready)
- DayEvents(t, lat, lon) Events: the solar day whose noon is nearest t (flips at solar midnight; time-zone independent)
- DayEventsOn(year, month, day, lat, lon) Events: by local-mean-solar calendar date
- SolarNoon(t, lon) time.Time
- Crossings(t, lat, lon, altDeg) (rise, set, state): state 0 normal, +1 always above, -1 always below
- NightRing(t, sunAltDeg, stepDeg) [][2]float64: closed CCW GeoJSON ring of the region where the sun is below sunAltDeg. Pole-containing caps run lon -180..180 and close over (±180, ±90). Pole-free caps (deep twilight near equinox) are a closed curve whose longitudes may run outside ±180.
- type NightSide {TimeMs, SubsolarLat, SubsolarLon, Sunset, Civil, Nautical, Astronomical rings}; NightSideAt(t, stepDeg); NightSide.GeoJSON() map[string]any (FeatureCollection of 4 Polygons with properties kind and sunAlt)
- ClampRing(ring, maxLatDeg): for map libraries that cannot project ±90 (MapLibre clamps by itself)
- type Moon {Illumination 0..1, Phase 0..1 (0 new, 0.5 full), AgeDays, Waxing, Name}; MoonAt(t)

## airview/internal/radio
Units: km, m, deg, hPa, °C, dBm, dBi, MHz.

Basics: FreqMHz=1090, WavelengthM, SpeedOfLightMPerS, BoltzmannJPerK, T0K=290. DB, FromDB, WattsToDBm, DBmToWatts, NoisePowerDBm(tempK, bwHz), NoiseTempFromNF(dB), NFFromNoiseTemp(K).

Horizon (k=0 means 4/3):
- RadioHorizonKm(hM,k), RadioHorizonApproxKm
- LineOfSightKm(antennaM, aircraftM, k), LineOfSightFtKm(antennaM, aircraftFt, k)
- MinAltitudeM(groundKm, antennaM, k)
- HorizonTable(antennaM,k) []BandRange{Band, Label, AltFt, LOSKm, LOSLoKm, LOSHiKm}: 7 rows
- HorizonShiftForK(antennaM, aircraftM, k) HorizonShift{K, RangeKm, StandardKm, DeltaKm, Factor, Trapping}
- HorizonFactor(k) = sqrt(k/(4/3)), capped at 3

Path loss:
- FSPLDB(distKm,freqMHz), FSPL1090DB(distKm)
- SpecificAttenuation(freqMHz,pHPa,tC,rhoGm3) (oxygen, water dB/km), ITU-R P.676-10 Annex 2
- type GasModel {OxygenDBPerKm, WaterDBPerKm, OxygenScaleKm, WaterScaleKm, RefHeightM}; NewGasModel(pHPa,tC,rhPct,heightM); StandardGas(); GasModel.PathLossDB(groundKm, antennaM, aircraftM, k); GaseousLossDB(groundKm, antennaM, aircraftM, k)

Transponders: TxPowerLowW=70, TxPowerMinW=125, TxPowerTypicalW=250, TxPowerMaxW=500; TransponderClasses() []TransponderClass{Name, PowerW, PowerDBm, Note}.

Antenna patterns:
- type Pattern interface { GainDBi(azimuthDeg, elevationDeg float64) float64 }
- Isotropic{GainDB}
- Omni: NewOmni(peakDBi, vBeamwidthDeg, tiltDeg, sidelobeDB, floorDB); HalfWaveVertical(); QuarterWaveVertical(); Collinear(gainDBi)
- Directional: NewDirectional(peakDBi, azimuthDeg, tiltDeg, hBW, vBW, frontToBackDB, sidelobeDB). cos^n main lobe, sidelobe envelope, F/B floor.
- Resolve(model.Antenna) PatternInfo{Kind, Directional, PeakGainDBi, AzimuthDeg, TiltDeg, HBeamwidthDeg, VBeamwidthDeg, FrontToBackDB, SidelobeDB, FloorDB, Estimated []string}. Derives missing gain/beamwidths/F-B (G = 0.7*41253/(θh*θv); yagi gain from elements = 10log10(N)+3; omni via McDonald). GainDBi == 0 means "not set". Estimated lists the model.Antenna JSON field names that were derived.
- PatternInfo.Pattern(); FromAntenna(model.Antenna) Pattern
- Helpers: OmniBeamwidthForGain, OmniGainForBeamwidth, DirectionalGainForBeamwidths, BeamwidthProductForGain, YagiGainForElements, BeamEfficiency=0.7
- Cuts: type Cut {AngleDeg, GainDBi []float64, Wrap bool}; AzimuthCut(p, elevationDeg, stepDeg) (0..<360); ElevationCut(p, azimuthDeg, stepDeg) (-90..90); ElevationCutRange(p, az, minEl, maxEl, step); Cut.Peak(); Cut.Beamwidth(downDB) (width, ok)

Link budget:
- type LinkParams {TxPowerW(250), TxAntennaGainDBi(0), BandwidthMHz(2), ReceiverNFDB(6), ReceiverSensitivityDBm(negative overrides NF), MinSNRDB(10), AntennaTempK(150), MiscLossDB, K(4/3), Gas(StandardGas)}; zero value = defaults; WithDefaults()
- AntennaHeightAMSL(model.Receiver, model.Antenna)
- NewLink(model.Antenna, antennaM, LinkParams) *Link. Fields: Info, Pattern, Params, AntennaM, TxPowerDBm, EIRPDBm, PolarizationLossDB (20 dB if Polarization=="horizontal"), FrontEndLossDB, LNAGainDB, LNANoiseFigDB, SystemTempK, SystemNFDB, NoiseFloorDBm, SensitivityDBm.
- Link.Budget(azimuthDeg, groundKm, aircraftAltM) Budget{geometry, FSPLDB, GasLossDB, RxAntennaGainDBi, ReceivedDBm, RxInputDBm, SNRDB, MarginDB, HorizonKm, LineOfSight, Decodable, ...}
- Link.ReceivedDBm(az, groundKm, altM)
- Link.MaxRangeKm(az, altM) (km, limit): LimitHorizon | LimitPower | LimitNone
- Link.RangeTable(az) []BandPrediction{Band, Label, AltFt, HorizonKm, RangeKm, Limit, MarginDB}
- Link.RangeByAzimuth(altM, stepDeg) RangeCut{AltM, HorizonKm, AngleDeg, RangeKm}; default step 2 deg matches coverage bearing bins
- Note: Budget/ReceivedDBm are in dBm at the antenna terminals, not dBFS. Comparing with measured RSSI needs a fitted offset.

Usage:
  l := radio.NewLink(ant, radio.AntennaHeightAMSL(cfg.Receiver, ant), radio.LinkParams{K: k})
  tab := l.RangeTable(ant.AzimuthDeg)
  b := l.Budget(brg, distKm, geo.FeetToMetres(altFt))

Atmosphere:
- SaturationVapourPressure(tC, pHPa), SaturationVapourPressureIce (pHPa=0 skips the enhancement factor)
- VapourPressureFromRH(tC, rhPct, pHPa), VapourPressureFromDewPoint(tdC, pHPa), RHFromDewPoint, DewPointFromRH, WaterVapourDensity(eHPa,tC)
- Refractivity(pHPa,tC,eHPa) N; RefractivityTerms (dry, wet); RefractivityRH(p,t,rh)
- ModifiedRefractivity(n,heightM) = N+157h; GradientNPerKm(n1,h1M,n2,h2M)
- KFactor(dNdh) = 157/(157+dN/dh); returns KMax=1000 for trapping (finite, JSON-safe); GradientForK(k); KStandard; MPerKm=157
- ClassifyGradient(dNdh): ClassSubRefraction (>0) | ClassStandard (0..-79) | ClassSuperRefraction (-79..-157) | ClassDucting (<=-157)
- type Level {HeightM, PressureHPa, TempC, RH}; Level.N()
- AnalyzeProfile([]Level) (Profile, ok); AnalyzePoints([]ProfilePoint{HeightM,N,M}) (Profile, ok)
- Profile {Points, Layers []Layer{BaseM,TopM,DNDH,DMDH,Class}, Ducts []Duct, SurfaceN, GradientNPerKm (lowest km), MinGradientNPerKm, MinGradientHeightM, K, Class}
- Duct {Kind: DuctSurface | DuctSurfaceBased | DuctElevated, BaseM, TopM, TrapBaseM, ThicknessM, StrengthM, MaxWavelengthM, CutoffMHz, Traps1090}
- DuctCutoff(thicknessM, strengthM, elevated) (lambdaMaxM, cutoffMHz)
- Assess(Profile) / AssessGradient(dNdh) Assessment{Index 0-100 (50 = standard, 85+ = ducting), Class, GradientNPerKm, K, HorizonFactor, Ducts, Ducting1090, Summary}
- These map directly to weather_obs columns n_surface, dn_dh, k_factor, duct.

ISA (geopotential = pressure altitude):
- ISA(altM) / ISAFt(altFt) ISAState{AltM, TempK, TempC, PressureHPa, DensityKgM3, SpeedOfSoundMS}
- ISATempC, ISAPressureHPa, PressureAltitudeM(pHPa), GeopotentialM, GeometricM, AirDensityKgM3
- SpeedOfSoundMS(tC), SpeedOfSoundKt(tC), TASFromMachKt(mach,tC), TempFromMachTAS(mach,tasKt) (tC, ok), ISADeviationC(pressureAltFt, oatC)
