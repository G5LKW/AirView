package refdata

// airlineTable lists ICAO three-letter operator designators with operator
// name, radiotelephony callsign, ISO country and a kind letter:
//
//	P passenger airline      C cargo airline
//	B business / charter / fractional operator
//	M military               G government, police, rescue, medical
//	X manufacturer, test, training and other
//
// One record per line: DESIGNATOR|Name|TELEPHONY|CC|K.
//
// Designators and telephony designators follow ICAO Doc 8585 as republished
// in FAA Order JO 7340.2 (chapter 3, section 3; edition 7340.2P was used for
// the last check) and the operator list of the tar1090-db project. Where the
// two disagree the FAA table wins. Names are the common trading names rather
// than the registered company names, and territories are given their own ISO
// code (Hong Kong, Taiwan, Faroe Islands ...) where ICAO lists the state.
//
// The table deliberately keeps well-known defunct operators and withdrawn
// designators (Air Berlin, Malev, WOW air, Swoop ...) because they still
// appear in recorded data. A designator that ICAO has re-assigned carries its
// current holder.
const airlineTable = `
AAL|American Airlines|AMERICAN|US|P
AAR|Asiana Airlines|ASIANA|KR|P
AAY|Allegiant Air|ALLEGIANT|US|P
ABD|Air Atlanta Icelandic|ATLANTA|IS|C
ABL|Air Busan|AIR BUSAN|KR|P
ABR|ASL Airlines Ireland|CONTRACT|IE|C
ABW|AirBridgeCargo Airlines|AIRBRIDGE CARGO|RU|C
ABX|ABX Air|ABEX|US|C
ABY|Air Arabia|ARABIA|AE|P
ACA|Air Canada|AIR CANADA|CA|P
ACI|Aircalin|AIRCALIN|NC|P
ADB|Antonov Airlines|ANTONOV BUREAU|UA|C
ADO|Air Do|AIR DO|JP|P
ADR|Adria Airways|ADRIA|SI|P
AEA|Air Europa|EUROPA|ES|P
AEE|Aegean Airlines|AEGEAN|GR|P
AFL|Aeroflot|AEROFLOT|RU|P
AFR|Air France|AIRFRANS|FR|P
AHK|Air Hong Kong|AIR HONG KONG|HK|C
AHO|Air Hamburg|AIR HAMBURG|DE|B
AHY|Azerbaijan Airlines|AZAL|AZ|P
AIB|Airbus Industrie|AIRBUS INDUSTRIE|FR|X
AIC|Air India|AIRINDIA|IN|P
AIQ|Thai AirAsia|THAI ASIA|TH|P
AIZ|Arkia Israeli Airlines|ARKIA|IL|P
AJT|Amerijet International|AMERIJET|US|C
ALK|SriLankan Airlines|SRILANKAN|LK|P
AMF|Ameriflight|AMFLIGHT|US|C
AMX|Aeromexico|AEROMEXICO|MX|P
ANA|All Nippon Airways|ALL NIPPON|JP|P
ANE|Air Nostrum|NOSTRU AIR|ES|P
ANG|Air Niugini|NIUGINI|PG|P
ANZ|Air New Zealand|NEW ZEALAND|NZ|P
AOJ|Avcon Jet|ASTERIX|AT|B
APJ|Peach Aviation|AIR PEACH|JP|P
ARA|Arik Air|ARIK AIR|NG|P
ARG|Aerolineas Argentinas|ARGENTINA|AR|P
ASA|Alaska Airlines|ALASKA|US|P
ASH|Mesa Airlines|AIR SHUTTLE|US|P
ASL|Air Serbia|AIR SERBIA|RS|P
ASQ|ExpressJet Airlines|ACEY|US|P
ASV|Air Seoul|AIR SEOUL|KR|P
ATN|Air Transport International|AIR TRANSPORT|US|C
AUA|Austrian Airlines|AUSTRIAN|AT|P
AUI|Ukraine International Airlines|UKRAINE INTERNATIONAL|UA|P
AUR|Aurigny Air Services|AYLINE|GG|P
AVA|Avianca|AVIANCA|CO|P
AWC|Titan Airways|ZAP|GB|P
AWI|Air Wisconsin|WISCONSIN|US|P
AWQ|Indonesia AirAsia|WAGON AIR|ID|P
AXB|Air India Express|EXPRESS INDIA|IN|P
AXM|AirAsia|RED CAP|MY|P
AZA|Alitalia|ALITALIA|IT|P
AZG|Silk Way West Airlines|SILK WEST|AZ|C
AZQ|Silk Way Airlines|SILK LINE|AZ|C
AZU|Azul Brazilian Airlines|AZUL|BR|P
BAV|Bamboo Airways|BAMBOO|VN|P
BAW|British Airways|SPEEDBIRD|GB|P
BBC|Biman Bangladesh Airlines|BANGLADESH|BD|P
BBD|Bluebird Nordic|BLUE CARGO|IS|C
BCS|European Air Transport Leipzig (DHL)|POSTMAN|DE|C
BCY|CityJet|CITY-IRELAND|IE|P
BEE|Flybe|JERSEY|GB|P
BEL|Brussels Airlines|BEELINE|BE|P
BER|Fly Air41 Airways (formerly Air Berlin)|BERLIN|HR|P
BGA|Airbus Transport International|BELOUGA|FR|C
BHL|Bristow Helicopters|BRISTOW|GB|B
BHS|Bahamasair|BAHAMAS|BS|P
BKP|Bangkok Airways|BANGKOK AIR|TH|P
BMS|Blue Air|BLUE MESSENGER|RO|P
BOE|Boeing|BOEING|US|X
BOV|Boliviana de Aviacion|BOLIVIANA|BO|P
BOX|AeroLogic|GERMAN CARGO|DE|C
BPA|Blue Panorama Airlines|BLUE PANORAMA|IT|P
BRU|Belavia|BELARUS AVIA|BY|P
BTI|airBaltic|AIRBALTIC|LV|P
BTK|Batik Air|BATIK|ID|P
BWA|Caribbean Airlines|CARIBBEAN AIRLINES|TT|P
CAI|Corendon Airlines|CORENDON|TR|P
CAL|China Airlines|DYNASTY|TW|P
CAO|Air China Cargo|AIRCHINA FREIGHT|CN|C
CAW|Comair (South Africa)|COMMERCIAL|ZA|P
CAY|Cayman Airways|CAYMAN|KY|P
CBJ|Beijing Capital Airlines|CAPITAL JET|CN|P
CCA|Air China|AIR CHINA|CN|P
CCM|Air Corsica|CORSICA|FR|P
CDG|Shandong Airlines|SHANDONG|CN|P
CEB|Cebu Pacific|CEBU AIR|PH|P
CES|China Eastern Airlines|CHINA EASTERN|CN|P
CFE|BA CityFlyer|FLYER|GB|P
CFG|Condor|CONDOR|DE|P
CFS|Empire Airlines|EMPIRE AIR|US|C
CHH|Hainan Airlines|HAINAN|CN|P
CJT|Cargojet Airways|CARGOJET|CA|C
CKK|China Cargo Airlines|CARGO KING|CN|C
CKS|Kalitta Air|CONNIE|US|C
CLH|Lufthansa CityLine|HANSALINE|DE|P
CLX|Cargolux|CARGOLUX|LU|C
CMP|Copa Airlines|COPA|PA|P
CND|Corendon Dutch Airlines|DUTCH CORENDON|NL|P
CPA|Cathay Pacific|CATHAY|HK|P
CPZ|Compass Airlines|COMPASS|US|P
CQH|Spring Airlines|AIR SPRING|CN|P
CRK|Hong Kong Airlines|BAUHINIA|HK|P
CRL|Corsair International|CORSAIR|FR|P
CSA|Czech Airlines|CSA LINES|CZ|P
CSC|Sichuan Airlines|SI CHUAN|CN|P
CSN|China Southern Airlines|CHINA SOUTHERN|CN|P
CSS|SF Airlines|SHUN FENG|CN|C
CSZ|Shenzhen Airlines|SHENZHEN AIR|CN|P
CTN|Croatia Airlines|CROATIA|HR|P
CTV|Citilink|SUPERGREEN|ID|P
CUB|Cubana de Aviacion|CUBANA|CU|P
CXA|Xiamen Airlines|XIAMEN AIR|CN|P
CYZ|China Postal Airlines|CHINA POST|CN|C
DAE|DHL Aero Expreso|YELLOW|PA|C
DAH|Air Algerie|AIR ALGERIE|DZ|P
DAL|Delta Air Lines|DELTA|US|P
DCS|DC Aviation|TWIN STAR|DE|B
DHK|DHL Air UK|WORLD EXPRESS|GB|C
DHX|DHL International Aviation ME|DILMUN|BH|C
DKH|Juneyao Airlines|AIR JUNEYAO|CN|P
DLA|Air Dolomiti|DOLOMITI|IT|P
DLH|Lufthansa|LUFTHANSA|DE|P
DTA|TAAG Angola Airlines|DTA|AO|P
DTR|Danish Air Transport|DANISH|DK|P
EAI|Emerald Airlines|GEMSTONE|IE|P
EDC|Air Charter Scotland|SALTIRE|GB|B
EDV|Endeavor Air|ENDEAVOR|US|P
EDW|Edelweiss Air|EDELWEISS|CH|P
EIN|Aer Lingus|SHAMROCK|IE|P
EJA|NetJets Aviation|EXECJET|US|B
EJM|Executive Jet Management|JET SPEED|US|B
EJU|easyJet Europe|ALPINE|AT|P
ELY|El Al Israel Airlines|ELAL|IL|P
ENT|Enter Air|ENTERAIR|PL|P
ENY|Envoy Air|ENVOY|US|P
ESR|Eastar Jet|EASTARJET|KR|P
ETD|Etihad Airways|ETIHAD|AE|P
ETH|Ethiopian Airlines|ETHIOPIAN|ET|P
EVA|EVA Air|EVA|TW|P
EVE|Iberojet (Evelop)|EVELOP|ES|P
EWG|Eurowings|EUROWINGS|DE|P
EXS|Jet2.com|CHANNEX|GB|P
EZE|Eastern Airways|EASTFLIGHT|GB|P
EZS|easyJet Switzerland|TOPSWISS|CH|P
EZY|easyJet|EASY|GB|P
FAB|First Air|FIRST AIR|CA|P
FBU|French Bee|FRENCH BEE|FR|P
FDB|flydubai|SKYDUBAI|AE|P
FDX|FedEx Express|FEDEX|US|C
FFT|Frontier Airlines|FRONTIER FLIGHT|US|P
FHY|Freebird Airlines|FREEBIRD AIR|TR|P
FIN|Finnair|FINNAIR|FI|P
FJI|Fiji Airways|FIJI|FJ|P
FLE|Flair Airlines|FLAIR|CA|P
FLI|Atlantic Airways|FAROELINE|FO|P
FPO|ASL Airlines France|FRENCH POST|FR|C
FPY|Play|PLAYER|IS|P
FWI|Air Caraibes|FRENCH WEST|FR|P
FYG|Flying Group|FLYING GROUP|BE|B
GAC|GlobeAir|DREAM TEAM|AT|B
GCR|Tianjin Airlines|BO HAI|CN|P
GEC|Lufthansa Cargo|LUFTHANSA CARGO|DE|C
GFA|Gulf Air|GULF AIR|BH|P
GIA|Garuda Indonesia|INDONESIA|ID|P
GJS|GoJet Airlines|LINDBERGH|US|P
GLO|Gol Linhas Aereas|GOL TRANSPORTE|BR|P
GMA|Gama Aviation|GAMA|GB|B
GOW|Go First|GOAIR|IN|P
GRL|Air Greenland|GREENLAND|GL|P
GTI|Atlas Air|GIANT|US|C
GWI|Germanwings|GERMAN WINGS|DE|P
HAL|Hawaiian Airlines|HAWAIIAN|US|P
HDA|Cathay Dragon|DRAGON|HK|P
HFY|Hi Fly|SKY FLYER|PT|P
HKE|HK Express|HONGKONG SHUTTLE|HK|P
HOP|HOP!|AIR HOP|FR|P
HVN|Vietnam Airlines|VIET NAM AIRLINES|VN|P
IAW|Iraqi Airways|IRAQI|IQ|P
IBE|Iberia|IBERIA|ES|P
IBK|Norwegian Air International|NORTRANS|IE|P
IBS|Iberia Express|IBEREXPRES|ES|P
ICE|Icelandair|ICEAIR|IS|P
ICL|CAL Cargo Air Lines|CAL|IL|C
ICV|Cargolux Italia|CARGOLUX ITALIA|IT|C
IGO|IndiGo|IFLY|IN|P
IJM|International Jet Management|JET MANAGEMENT|AT|B
IRA|Iran Air|IRANAIR|IR|P
IRM|Mahan Air|MAHAN AIR|IR|P
ISR|Israir Airlines|ISRAIR|IL|P
ITY|ITA Airways|ITARROW|IT|P
JAF|TUI fly Belgium|BEAUTY|BE|P
JAI|Jet Airways|JET AIRWAYS|IN|P
JAL|Japan Airlines|JAPANAIR|JP|P
JAT|JetSMART|ROCKSMART|CL|P
JBU|JetBlue Airways|JETBLUE|US|P
JEI|Jet Executive International Charter|JET EXECUTIVE|DE|B
JFA|Jetfly Aviation|MOSQUITO|LU|B
JIA|PSA Airlines|BLUE STREAK|US|P
JJA|Jeju Air|JEJU AIR|KR|P
JJP|Jetstar Japan|ORANGE LINER|JP|P
JNA|Jin Air|JIN AIR|KR|P
JST|Jetstar Airways|JETSTAR|AU|P
JTA|Japan Transocean Air|JAI OCEAN|JP|P
JTD|Jettime|JET TIME|DK|P
JTG|Jet Time (former designator)|JET TIME|DK|P
JTL|Jet Linx Aviation|JET LINX|US|B
JZA|Jazz Aviation|JAZZ|CA|P
JZR|Jazeera Airways|JAZEERA|KW|P
KAC|Kuwait Airways|KUWAITI|KW|P
KAL|Korean Air|KOREANAIR|KR|P
KFS|Kalitta Charters|KALITTA|US|C
KKK|AtlasGlobal|ATLASJET|TR|P
KLC|KLM Cityhopper|CITY|NL|P
KLM|KLM Royal Dutch Airlines|KLM|NL|P
KNE|flynas|FLYNAS|SA|P
KQA|Kenya Airways|KENYA|KE|P
KRP|Carpatair|CARPATAIR|RO|P
KZR|Air Astana|ASTANALINE|KZ|P
LAA|Libyan Airlines|LIBAIR|LY|P
LAM|LAM Mozambique Airlines|MOZAMBIQUE|MZ|P
LAN|LATAM Airlines Chile|LAN CHILE|CL|P
LAV|AlbaStar|ALBASTAR|ES|P
LBT|Nouvelair Tunisie|NOUVELAIR|TN|P
LCO|LATAM Cargo Chile|LAN CARGO|CL|C
LDM|Laudamotion|LAUDA MOTION|AT|P
LGL|Luxair|LUXAIR|LU|P
LNI|Lion Air|LION INTER|ID|P
LNK|Airlink|LINK|ZA|P
LNX|London Executive Aviation|LONEX|GB|B
LOG|Loganair|LOGAN|GB|P
LOT|LOT Polish Airlines|LOT|PL|P
LPE|LATAM Airlines Peru|LANPERU|PE|P
LRC|Avianca Costa Rica|LACSA|CR|P
LXA|Luxaviation|RED LION|LU|B
LXJ|Flexjet|FLEXJET|US|B
LZB|Bulgaria Air|FLYING BULGARIA|BG|P
MAA|MasAir|MAS CARGA|MX|C
MAH|Malev Hungarian Airlines|MALEV|HU|P
MAL|Morningstar Air Express|MORNINGSTAR|CA|C
MAS|Malaysia Airlines|MALAYSIAN|MY|P
MAU|Air Mauritius|AIRMAURITIUS|MU|P
MDA|Mandarin Airlines|MANDARIN|TW|P
MDG|Air Madagascar|AIR MADAGASCAR|MG|P
MEA|Middle East Airlines|CEDAR JET|LB|P
MGL|MIAT Mongolian Airlines|MONGOL AIR|MN|P
MLD|Air Moldova|AIR MOLDOVA|MD|P
MLM|Comlux Malta|LUXMALTA|MT|B
MNB|MNG Airlines|BLACK SEA|TR|C
MNO|Mango|TULCA|ZA|P
AKT|Canadian North|ARCTIC|CA|P
MPE|Canadian North (former designator)|EMPRESS|CA|P
MPH|Martinair|MARTINAIR|NL|C
MSC|Air Cairo|AIR CAIRO|EG|P
MSR|EgyptAir|EGYPTAIR|EG|P
MTN|Mountain Air Cargo|MOUNTAIN|US|C
MXD|Batik Air Malaysia|MALINDO|MY|P
MXY|Breeze Airways|MOXY|US|P
NAX|Norwegian Air Shuttle|NOR SHUTTLE|NO|P
NCA|Nippon Cargo Airlines|NIPPON CARGO|JP|C
NCR|National Airlines|NATIONAL CARGO|US|C
NIA|Nile Air|NILE BIRD|EG|P
NJE|NetJets Europe|FRACTION|PT|B
NKS|Spirit Airlines|SPIRIT WINGS|US|P
NOK|Nok Air|NOK AIR|TH|P
NOS|Neos|MOONFLOWER|IT|P
NOZ|Norwegian Air Shuttle AOC|NORDIC|NO|P
NPT|West Atlantic UK|NEPTUNE|GB|C
NSZ|Norwegian Air Sweden|REDNOSE|SE|P
NVR|Novair|NAVIGATOR|SE|P
OAE|Omni Air International|OMNI-EXPRESS|US|P
OAL|Olympic Air|OLYMPIC|GR|P
OAW|Helvetic Airways|HELVETIC|CH|P
OCN|Discover Airlines|OCEAN|DE|P
OHY|Onur Air|ONUR AIR|TR|P
OMA|Oman Air|OMAN AIR|OM|P
OMS|SalamAir|MAZOON|OM|P
PAC|Polar Air Cargo|POLAR|US|C
PAL|Philippine Airlines|PHILIPPINE|PH|P
PBD|Pobeda|POBEDA|RU|P
PCO|Pacific Coastal Airlines|PASCO|CA|P
PDT|Piedmont Airlines|PIEDMONT|US|P
PGA|Portugalia Airlines|PORTUGALIA|PT|P
PGT|Pegasus Airlines|SUNTURK|TR|P
PIA|Pakistan International Airlines|PAKISTAN|PK|P
PJS|Jet Aviation Business Jets|JETAVIATION|CH|B
POE|Porter Airlines|PORTER AIR|CA|P
PVG|Privilege Style|PRIVILEGE|ES|P
QAJ|Quick Air Jet Charter|DAGOBERT|DE|B
QFA|Qantas|QANTAS|AU|P
QLK|QantasLink (Sunstate / Eastern Australia)|QLINK|AU|P
QQE|Qatar Executive|QREX|QA|B
QTR|Qatar Airways|QATARI|QA|P
QXE|Horizon Air|HORIZON AIR|US|P
RAM|Royal Air Maroc|ROYALAIR MAROC|MA|P
RBA|Royal Brunei Airlines|BRUNEI|BN|P
REU|Air Austral|REUNION|RE|P
RJA|Royal Jordanian|JORDANIAN|JO|P
RNA|Nepal Airlines|ROYAL NEPAL|NP|P
ROJ|Royal Jet|ROYALJET|AE|B
ROT|TAROM|TAROM|RO|P
ROU|Air Canada Rouge|ROUGE|CA|P
RPA|Republic Airways|BRICKYARD|US|P
RUK|Ryanair UK|BLUEMAX|GB|P
RWD|RwandAir|RWANDAIR|RW|P
RXA|Regional Express|REX|AU|P
RYR|Ryanair|RYANAIR|IE|P
RYS|Buzz|MAGIC SUN|PL|P
RZO|Azores Airlines|AIR AZORES|PT|P
SAA|South African Airways|SPRINGBOK|ZA|P
SAS|Scandinavian Airlines|SCANDINAVIAN|SE|P
SBI|S7 Airlines|SIBERIAN AIRLINES|RU|P
SCX|Sun Country Airlines|SUN COUNTRY|US|P
SDM|Rossiya Airlines|ROSSIYA|RU|P
SDR|Sundair|SUNDAIR|DE|P
SEH|Sky Express|AIR CRETE|GR|P
SEJ|SpiceJet|SPICEJET|IN|P
SEY|Air Seychelles|SEYCHELLES|SC|P
SFJ|Star Flyer|STARFLYER|JP|P
SFR|Safair|CARGO|ZA|P
SHT|British Airways Shuttle|SHUTTLE|GB|P
SIA|Singapore Airlines|SINGAPORE|SG|P
SIL|Silver Airways|SILVER WINGS|US|P
SJX|Starlux Airlines|STARWALKER|TW|P
SKU|Sky Airline|AEROSKY|CL|P
SKW|SkyWest Airlines|SKYWEST|US|P
SKY|Skymark Airlines|SKYMARK|JP|P
SNJ|Solaseed Air|NEWSKY|JP|P
SOO|Southern Air|SOUTHERN AIR|US|C
SRR|Star Air|WHITESTAR|DK|C
SVA|Saudia|SAUDIA|SA|P
SVR|Ural Airlines|SVERDLOVSK AIR|RU|P
SVW|Global Jet Luxembourg|SILVER ARROWS|LU|B
SWA|Southwest Airlines|SOUTHWEST|US|P
SWG|Sunwing Airlines|SUNWING|CA|P
SWN|West Atlantic Sweden|AIR SWEDEN|SE|C
SWQ|iAero Airways (Swift Air)|SWIFTFLIGHT|US|P
SWR|Swiss International Air Lines|SWISS|CH|P
SWT|Swiftair|SWIFT|ES|C
SXD|SunExpress Deutschland|SUNRISE|DE|P
SXS|SunExpress|SUNEXPRESS|TR|P
SYR|Syrian Air|SYRIANAIR|SY|P
TAI|Avianca El Salvador (TACA)|TACA|SV|P
TAM|LATAM Airlines Brasil|TAM|BR|P
TAP|TAP Air Portugal|AIR PORTUGAL|PT|P
TAR|Tunisair|TUNAIR|TN|P
TAY|ASL Airlines Belgium|QUALITY|BE|C
TCV|Cabo Verde Airlines|CABOVERDE|CV|P
TCX|Thomas Cook Airlines|THOMAS COOK|GB|P
TFL|TUI fly Netherlands|ORANGE|NL|P
TGG|Tigerair Australia|TIGGOZ|AU|P
TGW|Scoot|SCOOTER|SG|P
THA|Thai Airways International|THAI|TH|P
THT|Air Tahiti Nui|TAHITI AIRLINES|PF|P
THY|Turkish Airlines|TURKISH|TR|P
TJS|Tyrolean Jet Services|TYROLJET|AT|B
TJT|Twin Jet|TWINJET|FR|P
TLM|Thai Lion Air|MENTARI|TH|P
TOM|TUI Airways|TOMJET|GB|P
TOW|AirTanker Services|TOWLINE|GB|P
TPA|Avianca Cargo (Tampa)|TAMPA|CO|C
TRA|Transavia|TRANSAVIA|NL|P
TSC|Air Transat|AIR TRANSAT|CA|P
TTW|Tigerair Taiwan|SMART CAT|TW|P
TUI|TUI fly Deutschland|TUI JET|DE|P
TVF|Transavia France|FRANCE SOLEIL|FR|P
TVJ|Thai VietJet Air|THAIVIET JET|TH|P
TVP|Smartwings Poland|JET TRAVEL|PL|P
TVS|Smartwings|SKYTRAVEL|CZ|P
TWB|T'way Air|TEEWAY|KR|P
UAE|Emirates|EMIRATES|AE|P
UAL|United Airlines|UNITED|US|P
UCA|CommutAir|COMMUTAIR|US|P
UPS|UPS Airlines|UPS|US|C
UTA|UTair|UTAIR|RU|P
UTY|Alliance Airlines|UNITY|AU|P
UZB|Uzbekistan Airways|UZBEK|UZ|P
VDA|Volga-Dnepr Airlines|VOLGA|RU|C
VIR|Virgin Atlantic|VIRGIN|GB|P
VIV|Viva Aerobus|AEROENLACES|MX|P
VJC|VietJet Air|VIETJETAIR|VN|P
VJT|VistaJet|VISTA JET|MT|B
VKG|Sunclass Airlines|VIKING|DK|P
VLG|Vueling|VUELING|ES|P
VOE|Volotea|VOLOTEA|ES|P
VOI|Volaris|VOLARIS|MX|P
VOZ|Virgin Australia|VELOCITY|AU|P
VTA|Air Tahiti|AIR TAHITI|PF|P
VTI|Vistara|VISTARA|IN|P
VXP|Avelo Airlines|AVELO|US|P
WEN|WestJet Encore|ENCORE|CA|P
WGN|Western Global Airlines|WESTERN GLOBAL|US|C
WIF|Wideroe|WIDEROE|NO|P
WIG|Wiggins Airways|WIGGINS AIRWAYS|US|C
WJA|WestJet|WESTJET|CA|P
WMT|Wizz Air Malta|WIZZ MALTA|MT|P
WOW|WOW air|WOW AIR|IS|P
WUK|Wizz Air UK|WIZZ GO|GB|P
WUP|Wheels Up|UP JET|US|B
WZZ|Wizz Air|WIZZ AIR|HU|P
XAX|AirAsia X|XANADU|MY|P
XLF|XL Airways France|STARWAY|FR|P

AAC|Army Air Corps|ARMYAIR|GB|M
AIO|United States Air Force (Air Chief)|AIR CHIEF|US|M
AME|Spanish Air Force|AIRMIL|ES|M
ASY|Royal Australian Air Force|AUSSIE|AU|M
AUH|Presidential Flight (Abu Dhabi)|SULTAN|AE|G
BAF|Belgian Air Force|BELGIAN AIRFORCE|BE|M
BAH|Bahrain Amiri Flight|BAHRAIN|BH|G
BRS|Brazilian Air Force|BRAZILIAN AIR FORCE|BR|M
CEF|Czech Air Force|CZECH AIR FORCE|CZ|M
CFC|Canadian Armed Forces|CANFORCE|CA|M
CHX|ADAC Luftrettung|CHRISTOPH|DE|G
CMB|United States Transportation Command|CAMBER|US|M
CNV|United States Navy|CONVOY|US|M
CTM|French Air and Space Force (COTAM)|COTAM|FR|M
DAF|Royal Danish Air Force|DANISH AIRFORCE|DK|M
DUB|Dubai Air Wing|DUBAI|AE|G
FAF|French Air and Space Force|FRENCH AIR FORCE|FR|M
FMY|French Army Light Aviation|FRENCH ARMY|FR|M
FNF|Finnish Air Force|FINNFORCE|FI|M
FNY|French Navy|FRENCH NAVY|FR|M
GAF|German Air Force|GERMAN AIR FORCE|DE|M
GAM|German Army|GERMAN ARMY|DE|M
GNY|German Navy|GERMAN NAVY|DE|M
HAF|Hellenic Air Force|HELLENIC AIR FORCE|GR|M
HLE|UK Helicopter Emergency Medical Services|HELIMED|GB|G
HUF|Hungarian Air Force|HUNGARIAN AIRFORCE|HU|M
IAM|Italian Air Force|ITALIAN AIRFORCE|IT|M
IRL|Irish Air Corps|IRISH|IE|M
KRC|New Zealand Defence Force|KIWI RESCUE|NZ|M
MJN|Royal Air Force of Oman|MAJAN|OM|M
NAF|Royal Netherlands Air Force|NETHERLANDS AIR FORCE|NL|M
NOW|Royal Norwegian Air Force|NORWEGIAN|NO|M
NRN|Royal Netherlands Navy|NETHERLANDS NAVY|NL|M
NVY|Royal Navy|NAVY|GB|M
PAT|United States Army (Priority Air Transport)|PAT|US|M
PLF|Polish Air Force|POLISH AIRFORCE|PL|M
QAF|Qatar Amiri Flight|AMIRI|QA|G
RCH|United States Air Force Air Mobility Command|REACH|US|M
RFR|Royal Air Force|RAFAIR|GB|M
ROF|Romanian Air Force|ROMAF|RO|M
RRR|Royal Air Force|ASCOT|GB|M
RSF|Royal Saudi Air Force|ARSAF|SA|M
SAZ|Swiss Air-Ambulance (Rega)|SWISS AMBULANCE|CH|G
SUI|Swiss Air Force|SWISS AIR FORCE|CH|M
SVF|Swedish Air Force|SWEDEFORCE|SE|M
UAF|United Arab Emirates Air Force|UNIFORCE|AE|M
UKP|UK National Police Air Service|POLICE|GB|G
ADN|Aero-Dienst|AERODIENST|DE|B
AEZ|Aeroitalia|AEROITALIA|IT|P
AFG|Ariana Afghan Airlines|ARIANA|AF|P
AJX|Air Japan|AIR JAPAN|JP|P
AMC|Air Malta|AIR MALTA|MT|P
AMU|Air Macau|AIR MACAU|MO|P
ARE|LATAM Airlines Colombia|LAN COLOMBIA|CO|P
AUL|Smartavia|ARCHANGELSK AIR|RU|P
AZE|Arcus Air|ARCUS AIR|DE|B
AZO|Azimuth Airlines|AZIMUTH DON|RU|P
AZV|Azur Air|AZUR AIR|RU|P
AZW|Air Zimbabwe|AIR ZIMBABWE|ZW|P
BGH|BH Air|BALKAN HOLIDAYS|BG|P
BLS|Bearskin Airlines|BEARSKIN|CA|P
BLX|TUI fly Nordic|BLUESCAN|SE|P
BMR|bmi regional|MIDLAND|GB|P
BOS|OpenSkies|MISTRAL|FR|P
BRO|2Excel Aviation|BROADSWORD|GB|B
BRX|Braathens Regional Airways|BRAATHENS|SE|P
BSK|Miami Air International|BISCAYNE|US|P
BTQ|Boutique Air|BOUTIQUE|US|P
BUC|European Air Charter|EUROCHARTER|BG|P
BVR|ACM Air Charter|BAVARIAN|DE|B
CDC|Loong Air|LOONG AIR|CN|P
CGZ|Colorful Guizhou Airlines|COLORFUL|CN|P
CHB|China West Air|WEST CHINA|CN|P
CHQ|Chautauqua Airlines|CHAUTAUQUA|US|P
CJX|Jiangxi Air|AIR CRANE|CN|P
CLF|Centreline Aviation|CLIFTON|GB|B
CLG|Chalair Aviation|CHALLAIR|FR|P
CLU|CargoLogicAir|FIREBIRD|GB|C
CNF|Canaryfly|CANARY|ES|P
CNK|Sunwest Aviation|CHINOOK|CA|B
COM|Comair|COMAIR|US|P
CSG|China Southern Cargo|SOUTHERN CARGO|CN|C
CSH|Shanghai Airlines|SHANGHAI AIR|CN|P
CTJ|Tianjin Air Cargo|TIANJIN CARGO|CN|C
CUA|China United Airlines|CHINA UNITED|CN|P
CVK|Cavok Air|CARGO LINE|UA|C
CXB|Comlux Aruba|STARLUX|AW|B
CYP|Cyprus Airways|CYPRUS|CY|P
DJT|La Compagnie (Dreamjet)|DREAMJET|FR|P
DNU|DAT LT|DANU|LT|P
DPJ|Delta Private Jets|JET CARD|US|B
DSO|Dassault Falcon Service|DASSAULT|FR|B
EFW|BA Euroflyer|GRIFFIN|GB|P
EPA|Donghai Airlines|DONGHAI AIR|CN|P
EUK|Aer Lingus UK|GREEN FLIGHT|GB|P
FAD|flyadeal|ADEAL|SA|P
FBZ|Flybondi|BONDI|AR|P
FDA|Fuji Dream Airlines|FUJI DREAM|JP|P
FEG|FlyEgypt|SKY EGYPT|EG|P
FIA|FlyOne|FIA AIRLINES|MD|P
FLJ|Flexjet Operations (UK)|FLEXAIR|GB|B
CHG|Challenge Airlines (BE)|CHALLENGE|BE|C
FRH|Challenge Airlines (BE) (former designator)|CHALLENGE|BE|C
FZA|Fuzhou Airlines|STRAIT AIR|CN|P
GDC|Grand China Air|GRAND CHINA|CN|P
GJT|GetJet Airlines|GETJET|LT|P
GLA|Great Lakes Airlines|LAKES AIR|US|P
GLF|Gulfstream Aerospace|GULFSTREAM TEST|US|X
GXA|Global Crossing Airlines|GEMINI|US|P
GZP|Gazpromavia|GAZPROM|RU|P
HBH|Hebei Airlines|HEBEI AIR|CN|P
HBR|Hebridean Air Services|HEBRIDEAN|GB|P
HKS|CHC Helikopter Service|HELIBUS|NO|B
HRT|Chartright Air|CHARTRIGHT|CA|B
HST|Heston Airlines|HESTON|LT|P
HXA|China Express Airlines|CHINA EXPRESS|CN|P
HYP|Hyperion Aviation|HYPERION|MT|B
IBX|Ibex Airlines|IBEX|JP|P
IFA|FAI rent-a-jet|RED ANGEL|DE|B
IRC|Iran Aseman Airlines|ASEMAN|IR|P
IYE|Yemenia|YEMENI|YE|P
IZG|Zagros Airlines|ZAGROS|IR|P
JAS|Jet Aviation Flight Services|JET SETTER|US|B
JDI|Jet Story|JEDI|PL|B
JRE|flyExclusive|JET SHARE|US|B
JYH|9 Air|TRANS JADE|CN|P
KAP|Cape Air|CAIR|US|P
KFA|KF Cargo (Kelowna Flightcraft)|FLIGHTCRAFT|CA|C
KHV|Air Cambodia|CAMBODIA AIR|KH|P
KLJ|KlasJet|CLASS LINE|LT|B
KMF|Kam Air|KAMGAR|AF|P
KNA|Kunming Airlines|KUNMING AIR|CN|P
KRE|Aerosucre|AEROSUCRE|CO|C
LAE|LATAM Cargo Colombia|LANCO|CO|C
LBQ|Quest Diagnostics|LABQUEST|US|B
LHX|Lufthansa City Airlines|CITY AIR|DE|P
LKE|Lucky Air|LUCKY AIR|CN|P
LLP|Small Planet Airlines Poland|SKYPOL|PL|P
LOF|Trans States Airlines|WATERSKI|US|P
LSI|MSC Air Cargo (formerly Aliscargo Airlines)|POSEIDON|IT|C
LYM|Key Lime Air|KEY LIME|US|C
LYX|SmartLynx Airlines Malta|MALTA CAT|MT|P
MAC|Air Arabia Maroc|ARABIA MAROC|MA|P
MGX|Montenegro Airlines|MONTAIR|ME|P
MHO|Mokulele Airlines|MAHALO|US|P
MHV|MHS Aviation|SNOWCAP|DE|B
MMA|Myanmar Airways International|MYANMAR|MM|P
MMD|Air Alsie|MERMAID|DK|B
MON|Monarch Airlines|MONARCH|GB|P
MSX|EgyptAir Cargo|EGYPTAIR CARGO|EG|C
NAC|Northern Air Cargo|YUKON|US|C
NHZ|NHV Helicopters|GOLD|GB|B
NMB|Air Namibia|NAMIBIA|NA|P
NSE|SATENA|SATENA|CO|P
NWK|Network Aviation|NETLINK|AU|P
NWS|Nordwind Airlines|NORDLAND|RU|P
NZM|Mount Cook Airline|MOUNTCOOK|NZ|P
OKA|Okay Airways|OKAYJET|CN|P
OPT|Flight Options|OPTIONS|US|B
OXF|Oxford Aviation Academy|OXFORD|GB|X
PCH|Pilatus Aircraft|PILATUS WINGS|CH|X
PDG|PDG Helicopters|OSPREY|GB|B
GRB|Phoenix Air Group|GRAY BIRD|US|B
PTN|Platoon Aviation|PLATOON|DE|B
PWF|Private Wings|PRIVATE WINGS|DE|B
QSM|Qeshm Air|QESHM AIR|IR|P
RLU|RusLine|RUSLINE AIR|RU|P
RSY|I-Fly|RUSSIAN SKY|RU|P
RXI|Riyadh Air|RIYADH AIR|SA|P
SJO|Spring Japan|JEY SPRING|JP|P
SKK|ASKY Airlines|ASKY AIRLINE|TG|P
SLI|Aeromexico Connect|COSTERA|MX|P
SNC|Air Cargo Carriers|NIGHT CARGO|US|C
SRN|SprintAir|SPRINTAIR|PL|C
STK|Stobart Air|STOBART AIR|IE|P
SUD|Sudan Airways|SUDANAIR|SD|P
SWU|SkyAlps|SKYALPS|IT|P
SXN|SaxonAir|SAXONAIR|GB|B
SZS|SAS Connect|SPINNAKER|IE|P
TAE|TAME|TAME|EC|P
TBA|Tibet Airlines|TIBET|CN|P
TGZ|Georgian Airways|TAMAZI|GE|P
TJK|Tajik Air|TAJIKAIR|TJ|P
TRS|AirTran Airways|CITRUS|US|P
TUA|Turkmenistan Airlines|TURKMENISTAN|TM|P
TVL|Smartwings Hungary|TRAVEL SERVICE|HU|P
TWI|Tailwind Airlines|TAILWIND|TR|P
TWY|Sunset Aviation (Solairus)|TWILIGHT|US|B
TYA|NordStar|TAIMYR|RU|P
UBA|Myanmar National Airlines|UNIONAIR|MM|P
UBG|US-Bangla Airlines|BANGLASTAR|BD|P
UEA|Chengdu Airlines|UNITED EAGLE|CN|P
UGD|Uganda Airlines|CRESTED|UG|P
UJC|Ultimate Jetcharters|ULTIMATE|US|B
UKL|Ukraine Air Alliance|UKRAINE ALLIANCE|UA|C
VCV|Conviasa|CONVIASA|VE|P
VJA|Vista America|ICONIC|US|B
VRD|Virgin America|REDWOOD|US|P
VTE|Contour Airlines|VOLUNTEER|US|P
WAZ|Wizz Air Abu Dhabi|WIZZ SKY|AE|P
GER|German Airways|GERMAN EAGLE|DE|P
WDL|WDL Aviation|WDL|DE|P
WHE|Leonardo Helicopters UK|WESTLAND|GB|X
WHT|White Airways|WHITEJET|PT|P
WSN|Advanced Air|WINGSPAN|US|P
WSW|Swoop|SWOOP|CA|P
XOJ|XOJET Aviation|EXOJET|US|B
XSR|Airshare|AIRSHARE|US|B

BPO|German Federal Police Air Wing|PIROL|DE|G
EGY|Egyptian Air Force||EG|M
FAG|Argentine Air Force|FUAER|AR|M
HRZ|Croatian Air Force|CROATIAN AIRFORCE|HR|M
HVK|Turkish Air Force|TURKISH AIRFORCE|TR|M
IAF|Israeli Air Force||IL|M
KRF|UK Royal / VIP flights (RAF)|KITTYHAWK|GB|M
LOS|RAF Lossiemouth|LOSSIE|GB|M
MMF|NATO Multinational MRTT Fleet|MULTI|NL|M
MRH|RAF Marham|MARHAM|GB|M
NGR|Nigerian Air Force|NIGERIAN AIRFORCE|NG|M
NOH|RAF No. 32 (The Royal) Squadron|NORTHOLT|GB|M
RHD|Bond Air Services|RED HEAD|GB|G
SHF|RAF Support Helicopter Force|VORTEX|GB|M
SSG|Slovak Government Flight Service|SLOVAK GOVERNMENT|SK|G
TYW|Tyrol Air Ambulance|TYROL AMBULANCE|AT|G
`
