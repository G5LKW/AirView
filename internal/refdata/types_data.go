package refdata

// typeTable lists common ICAO aircraft type designators (ICAO Doc 8643).
//
// One record per line: DESIGNATOR|Manufacturer|Model|DESC|WTC|ROLE
//
//	DESC  ICAO description code: category letter (L landplane, S seaplane,
//	      A amphibian, H helicopter, G gyrocopter, T tilt-rotor), engine
//	      count, engine type (J jet, T turboprop/turboshaft, P piston,
//	      E electric, R rocket). "-" when the designator has none.
//	WTC   ICAO wake turbulence category: L light, M medium, H heavy, J super.
//	ROLE  AirView grouping, see TypeRole.
//
// DESC and WTC were cross-checked against the Doc 8643 data republished by
// the tar1090-db project.
const typeTable = `
A19N|Airbus|A319neo|L2J|M|A
A20N|Airbus|A320neo|L2J|M|A
A21N|Airbus|A321neo|L2J|M|A
A318|Airbus|A318|L2J|M|A
A319|Airbus|A319|L2J|M|A
A320|Airbus|A320|L2J|M|A
A321|Airbus|A321|L2J|M|A
A306|Airbus|A300-600|L2J|H|A
A30B|Airbus|A300B2/B4|L2J|H|A
A310|Airbus|A310|L2J|H|A
A332|Airbus|A330-200|L2J|H|A
A333|Airbus|A330-300|L2J|H|A
A337|Airbus|BelugaXL (A330-743L)|L2J|H|F
A338|Airbus|A330-800neo|L2J|H|A
A339|Airbus|A330-900neo|L2J|H|A
A342|Airbus|A340-200|L4J|H|A
A343|Airbus|A340-300|L4J|H|A
A345|Airbus|A340-500|L4J|H|A
A346|Airbus|A340-600|L4J|H|A
A359|Airbus|A350-900|L2J|H|A
A35K|Airbus|A350-1000|L2J|H|A
A388|Airbus|A380-800|L4J|J|A
A3ST|Airbus|Beluga (A300-600ST)|L2J|H|F
A400|Airbus|A400M Atlas|L4T|H|M
BCS1|Airbus|A220-100|L2J|M|A
BCS3|Airbus|A220-300|L2J|M|A
B37M|Boeing|737 MAX 7|L2J|M|A
B38M|Boeing|737 MAX 8|L2J|M|A
B39M|Boeing|737 MAX 9|L2J|M|A
B3XM|Boeing|737 MAX 10|L2J|M|A
B712|Boeing|717-200|L2J|M|A
B721|Boeing|727-100|L3J|M|A
B722|Boeing|727-200|L3J|M|A
B732|Boeing|737-200|L2J|M|A
B733|Boeing|737-300|L2J|M|A
B734|Boeing|737-400|L2J|M|A
B735|Boeing|737-500|L2J|M|A
B736|Boeing|737-600|L2J|M|A
B737|Boeing|737-700|L2J|M|A
B738|Boeing|737-800|L2J|M|A
B739|Boeing|737-900|L2J|M|A
B741|Boeing|747-100|L4J|H|A
B742|Boeing|747-200|L4J|H|A
B743|Boeing|747-300|L4J|H|A
B744|Boeing|747-400|L4J|H|A
B748|Boeing|747-8|L4J|H|A
B74S|Boeing|747SP|L4J|H|A
BLCF|Boeing|747-400 LCF Dreamlifter|L4J|H|F
B752|Boeing|757-200|L2J|M|A
B753|Boeing|757-300|L2J|M|A
B762|Boeing|767-200|L2J|H|A
B763|Boeing|767-300|L2J|H|A
B764|Boeing|767-400|L2J|H|A
B772|Boeing|777-200|L2J|H|A
B77L|Boeing|777-200LR / 777F|L2J|H|A
B773|Boeing|777-300|L2J|H|A
B77W|Boeing|777-300ER|L2J|H|A
B778|Boeing|777-8|L2J|H|A
B779|Boeing|777-9|L2J|H|A
B788|Boeing|787-8 Dreamliner|L2J|H|A
B789|Boeing|787-9 Dreamliner|L2J|H|A
B78X|Boeing|787-10 Dreamliner|L2J|H|A
B703|Boeing|707-300|L4J|H|A
B720|Boeing|720|L4J|M|A
DC10|McDonnell Douglas|DC-10|L3J|H|A
MD11|McDonnell Douglas|MD-11|L3J|H|A
MD81|McDonnell Douglas|MD-81|L2J|M|A
MD82|McDonnell Douglas|MD-82|L2J|M|A
MD83|McDonnell Douglas|MD-83|L2J|M|A
MD87|McDonnell Douglas|MD-87|L2J|M|A
MD88|McDonnell Douglas|MD-88|L2J|M|A
MD90|McDonnell Douglas|MD-90|L2J|M|A
DC86|Douglas|DC-8-60|L4J|H|A
DC87|Douglas|DC-8-70|L4J|H|A
DC93|Douglas|DC-9-30|L2J|M|A
DC3|Douglas|DC-3 / C-47|L2P|M|A
DC6|Douglas|DC-6|L4P|M|A
L101|Lockheed|L-1011 TriStar|L3J|H|A
L188|Lockheed|L-188 Electra|L4T|M|A
CONC|Aerospatiale-BAC|Concorde|L4J|H|A
E135|Embraer|ERJ-135|L2J|M|R
E145|Embraer|ERJ-145|L2J|M|R
E45X|Embraer|ERJ-145XR|L2J|M|R
E170|Embraer|E170|L2J|M|R
E75S|Embraer|E175 (short wing)|L2J|M|R
E75L|Embraer|E175 (long wing)|L2J|M|R
E190|Embraer|E190|L2J|M|R
E195|Embraer|E195|L2J|M|R
E290|Embraer|E190-E2|L2J|M|R
E295|Embraer|E195-E2|L2J|M|R
E120|Embraer|EMB-120 Brasilia|L2T|M|R
E110|Embraer|EMB-110 Bandeirante|L2T|L|R
CRJ1|Bombardier|CRJ100|L2J|M|R
CRJ2|Bombardier|CRJ200|L2J|M|R
CRJ7|Bombardier|CRJ700|L2J|M|R
CRJ9|Bombardier|CRJ900|L2J|M|R
CRJX|Bombardier|CRJ1000|L2J|M|R
DH8A|De Havilland Canada|Dash 8-100|L2T|M|R
DH8B|De Havilland Canada|Dash 8-200|L2T|M|R
DH8C|De Havilland Canada|Dash 8-300|L2T|M|R
DH8D|De Havilland Canada|Dash 8-400|L2T|M|R
DHC6|De Havilland Canada|DHC-6 Twin Otter|L2T|L|R
DHC7|De Havilland Canada|DHC-7 Dash 7|L4T|M|R
AT43|ATR|ATR 42-300|L2T|M|R
AT45|ATR|ATR 42-500|L2T|M|R
AT46|ATR|ATR 42-600|L2T|M|R
AT72|ATR|ATR 72-200|L2T|M|R
AT73|ATR|ATR 72-210|L2T|M|R
AT75|ATR|ATR 72-500|L2T|M|R
AT76|ATR|ATR 72-600|L2T|M|R
SF34|Saab|340|L2T|M|R
SB20|Saab|2000|L2T|M|R
F27|Fokker|F27 Friendship|L2T|M|R
F28|Fokker|F28 Fellowship|L2J|M|R
F50|Fokker|50|L2T|M|R
F70|Fokker|70|L2J|M|R
F100|Fokker|100|L2J|M|R
B461|British Aerospace|BAe 146-100|L4J|M|R
B462|British Aerospace|BAe 146-200|L4J|M|R
B463|British Aerospace|BAe 146-300|L4J|M|R
RJ70|Avro|RJ70|L4J|M|R
RJ85|Avro|RJ85|L4J|M|R
RJ1H|Avro|RJ100|L4J|M|R
JS31|British Aerospace|Jetstream 31|L2T|L/M|R
JS32|British Aerospace|Jetstream 32|L2T|M|R
JS41|British Aerospace|Jetstream 41|L2T|M|R
ATP|British Aerospace|ATP|L2T|M|R
A748|Hawker Siddeley|HS 748|L2T|M|R
D228|Dornier|228|L2T|L|R
D328|Dornier|328|L2T|M|R
J328|Dornier|328JET|L2J|M|R
SW4|Fairchild|Metro / Merlin IV|L2T|L/M|R
B190|Beechcraft|1900|L2T|M|R
L410|Let|L-410 Turbolet|L2T|L|R
SH33|Short|330|L2T|M|R
SH36|Short|360|L2T|M|R
SC7|Short|SC.7 Skyvan|L2T|L|R
C212|CASA|C-212 Aviocar|L2T|M|R
BN2P|Britten-Norman|BN-2 Islander|L2P|L|R
BN2T|Britten-Norman|BN-2T Turbine Islander|L2T|L|R
TRIS|Britten-Norman|Trislander|L3P|L|R
SU95|Sukhoi|Superjet 100|L2J|M|R
AJ27|COMAC|ARJ21|L2J|M|R
C919|COMAC|C919|L2J|M|A
MA60|Xian|MA60|L2T|M|R
AN24|Antonov|An-24|L2T|M|R
AN26|Antonov|An-26|L2T|M|F
AN28|Antonov|An-28|L2T|L|R
AN30|Antonov|An-30|L2T|M|R
AN32|Antonov|An-32|L2T|M|F
AN72|Antonov|An-72 / An-74|L2J|M|F
AN12|Antonov|An-12|L4T|M|F
A124|Antonov|An-124 Ruslan|L4J|H|F
A225|Antonov|An-225 Mriya|L6J|H|F
A148|Antonov|An-148|L2J|M|R
A158|Antonov|An-158|L2J|M|R
AN2|Antonov|An-2|L1P|L|T
IL18|Ilyushin|Il-18|L4T|M|A
IL62|Ilyushin|Il-62|L4J|H|A
IL76|Ilyushin|Il-76|L4J|H|F
IL96|Ilyushin|Il-96|L4J|H|A
T134|Tupolev|Tu-134|L2J|M|A
T154|Tupolev|Tu-154|L3J|M|A
T204|Tupolev|Tu-204 / Tu-214|L2J|M|A
YK40|Yakovlev|Yak-40|L3J|M|R
YK42|Yakovlev|Yak-42|L3J|M|R
C25A|Cessna|Citation CJ2|L2J|L|B
C25B|Cessna|Citation CJ3|L2J|L|B
C25C|Cessna|Citation CJ4|L2J|M|B
C25M|Cessna|Citation M2|L2J|L|B
C500|Cessna|Citation I|L2J|L|B
C501|Cessna|Citation I/SP|L2J|L|B
C510|Cessna|Citation Mustang|L2J|L|B
C525|Cessna|CitationJet / CJ1|L2J|L|B
C550|Cessna|Citation II / Bravo|L2J|L|B
C560|Cessna|Citation V / Ultra / Encore|L2J|M|B
C56X|Cessna|Citation Excel / XLS|L2J|M|B
C650|Cessna|Citation III / VI / VII|L2J|M|B
C680|Cessna|Citation Sovereign|L2J|M|B
C68A|Cessna|Citation Latitude|L2J|M|B
C700|Cessna|Citation Longitude|L2J|M|B
C750|Cessna|Citation X|L2J|M|B
CL30|Bombardier|Challenger 300|L2J|M|B
CL35|Bombardier|Challenger 350|L2J|M|B
CL60|Bombardier|Challenger 600 / 601 / 604 / 605 / 650|L2J|M|B
GLEX|Bombardier|Global Express / Global 6000|L2J|M|B
GL5T|Bombardier|Global 5000|L2J|M|B
GL7T|Bombardier|Global 7500|L2J|M|B
LJ31|Learjet|31|L2J|M|B
LJ35|Learjet|35|L2J|M|B
LJ40|Learjet|40|L2J|M|B
LJ45|Learjet|45|L2J|M|B
LJ55|Learjet|55|L2J|M|B
LJ60|Learjet|60|L2J|M|B
LJ75|Learjet|75|L2J|M|B
GLF2|Gulfstream|II|L2J|M|B
GLF3|Gulfstream|III|L2J|M|B
GLF4|Gulfstream|IV / G450|L2J|M|B
GLF5|Gulfstream|V / G550|L2J|M|B
GLF6|Gulfstream|G650|L2J|M|B
GA5C|Gulfstream|G500|L2J|M|B
GA6C|Gulfstream|G600|L2J|M|B
GA7C|Gulfstream|G700|L2J|M|B
G150|Gulfstream|G150|L2J|M|B
G280|Gulfstream|G280|L2J|M|B
GALX|Gulfstream|G200 Galaxy|L2J|M|B
ASTR|IAI|1125 Astra / G100|L2J|M|B
F900|Dassault|Falcon 900|L3J|M|B
F2TH|Dassault|Falcon 2000|L2J|M|B
FA10|Dassault|Falcon 10|L2J|M|B
FA20|Dassault|Falcon 20|L2J|M|B
FA50|Dassault|Falcon 50|L3J|M|B
FA6X|Dassault|Falcon 6X|L2J|M|B
FA7X|Dassault|Falcon 7X|L3J|M|B
FA8X|Dassault|Falcon 8X|L3J|M|B
H25B|Hawker|800 / 800XP / 900XP|L2J|M|B
H25C|Hawker|1000|L2J|M|B
HA4T|Hawker|4000|L2J|M|B
BE40|Beechcraft|Beechjet 400 / Hawker 400|L2J|M|B
PRM1|Beechcraft|Premier I|L2J|L|B
E50P|Embraer|Phenom 100|L2J|L|B
E55P|Embraer|Phenom 300|L2J|M|B
E35L|Embraer|Legacy 600 / 650|L2J|M|B
E545|Embraer|Legacy 450 / Praetor 500|L2J|M|B
E550|Embraer|Legacy 500 / Praetor 600|L2J|M|B
HDJT|Honda|HA-420 HondaJet|L2J|L|B
SF50|Cirrus|SF50 Vision Jet|L1J|L|B
EA50|Eclipse|500|L2J|L|B
PC24|Pilatus|PC-24|L2J|M|B
P180|Piaggio|P.180 Avanti|L2T|L|B
PC12|Pilatus|PC-12|L1T|L|T
C208|Cessna|208 Caravan|L1T|L|T
TBM7|Socata|TBM 700|L1T|L|T
TBM8|Socata|TBM 850|L1T|L|T
TBM9|Daher|TBM 900 series|L1T|L|T
P46T|Piper|PA-46 Meridian / M500 / M600|L1T|L|T
KODI|Quest|Kodiak 100|L1T|L|T
BE20|Beechcraft|King Air 200|L2T|L/M|T
B350|Beechcraft|King Air 350|L2T|L/M|T
BE9L|Beechcraft|King Air 90|L2T|L|T
BE99|Beechcraft|99 Airliner|L2T|L|T
C441|Cessna|441 Conquest II|L2T|L|T
C425|Cessna|425 Conquest I|L2T|L|T
PAY2|Piper|PA-31T Cheyenne II|L2T|L|T
MU2|Mitsubishi|MU-2|L2T|L|T
AC90|Aero Commander|690 Turbo Commander|L2T|L|T
C150|Cessna|150|L1P|L|G
C152|Cessna|152|L1P|L|G
C172|Cessna|172 Skyhawk|L1P|L|G
C177|Cessna|177 Cardinal|L1P|L|G
C182|Cessna|182 Skylane|L1P|L|G
C206|Cessna|206 Stationair|L1P|L|G
C210|Cessna|210 Centurion|L1P|L|G
C310|Cessna|310|L2P|L|G
C337|Cessna|337 Skymaster|L2P|L|G
C340|Cessna|340|L2P|L|G
C402|Cessna|402|L2P|L|G
C414|Cessna|414 Chancellor|L2P|L|G
C421|Cessna|421 Golden Eagle|L2P|L|G
P28A|Piper|PA-28 Cherokee / Warrior / Archer|L1P|L|G
P28B|Piper|PA-28-236 Dakota|L1P|L|G
P28R|Piper|PA-28R Arrow|L1P|L|G
P28T|Piper|PA-28RT Turbo Arrow|L1P|L|G
PA18|Piper|PA-18 Super Cub|L1P|L|G
PA22|Piper|PA-22 Tri-Pacer|L1P|L|G
PA24|Piper|PA-24 Comanche|L1P|L|G
PA31|Piper|PA-31 Navajo|L2P|L|G
PA32|Piper|PA-32 Cherokee Six|L1P|L|G
P32R|Piper|PA-32R Lance / Saratoga|L1P|L|G
PA34|Piper|PA-34 Seneca|L2P|L|G
PA38|Piper|PA-38 Tomahawk|L1P|L|G
PA44|Piper|PA-44 Seminole|L2P|L|G
PA46|Piper|PA-46 Malibu / Mirage|L1P|L|G
BE33|Beechcraft|33 Debonair / Bonanza|L1P|L|G
BE35|Beechcraft|35 Bonanza|L1P|L|G
BE36|Beechcraft|36 Bonanza|L1P|L|G
BE55|Beechcraft|55 Baron|L2P|L|G
BE58|Beechcraft|58 Baron|L2P|L|G
BE60|Beechcraft|60 Duke|L2P|L|G
BE76|Beechcraft|76 Duchess|L2P|L|G
M20P|Mooney|M20|L1P|L|G
M20T|Mooney|M20 (turbocharged)|L1P|L|G
SR20|Cirrus|SR20|L1P|L|G
SR22|Cirrus|SR22|L1P|L|G
S22T|Cirrus|SR22T|L1P|L|G
DV20|Diamond|DA20 / DV20 Katana|L1P|L|G
DA40|Diamond|DA40 Diamond Star|L1P|L|G
DA42|Diamond|DA42 Twin Star|L2P|L|G
DA50|Diamond|DA50|L1P|L|G
DA62|Diamond|DA62|L2P|L|G
AA5|Grumman American|AA-5 Traveler / Tiger|L1P|L|G
DR40|Robin|DR400|L1P|L|G
TOBA|Socata|TB10 Tobago|L1P|L|G
TB20|Socata|TB20 Trinidad|L1P|L|G
G115|Grob|G 115|L1P|L|G
A210|Aquila|A210 / A211|L1P|L|G
RV6|Van's|RV-6|L1P|L|G
RV7|Van's|RV-7|L1P|L|G
RV8|Van's|RV-8|L1P|L|G
RV9|Van's|RV-9|L1P|L|G
RV10|Van's|RV-10|L1P|L|G
E300|Extra|EA-300|L1P|L|G
PTS2|Pitts|S-2 Special|L1P|L|G
CP10|Mudry|CAP 10|L1P|L|G
YK52|Yakovlev|Yak-52|L1P|L|G
DH82|De Havilland|DH.82 Tiger Moth|L1P|L|G
DHC1|De Havilland Canada|DHC-1 Chipmunk|L1P|L|G
DHC2|De Havilland Canada|DHC-2 Beaver|L1P|L|G
DHC3|De Havilland Canada|DHC-3 Otter|L1P|L|G
T6|North American|T-6 Texan / Harvard|L1P|L|G
P51|North American|P-51 Mustang|L1P|L|G
SPIT|Supermarine|Spitfire|L1P|L|G
HURI|Hawker|Hurricane|L1P|L|G
LANC|Avro|Lancaster|L4P|M|G
B17|Boeing|B-17 Flying Fortress|L4P|M|G
C42|Comco Ikarus|C42|L1P|L|L
EV97|Evektor|EV-97 Eurostar|L1P|L|L
PIVI|Pipistrel|Virus|L1P|L|L
GLID|(generic)|Glider / sailplane|-|-|L
ULAC|(generic)|Microlight / ultralight|-|-|L
BALL|(generic)|Balloon|-|-|L
GYRO|(generic)|Gyrocopter|-|-|L
SHIP|(generic)|Airship|-|-|L
R22|Robinson|R22|H1P|L|H
R44|Robinson|R44|H1P|L|H
R66|Robinson|R66|H1T|L|H
G2CA|Guimbal|Cabri G2|H1P|L|H
B06|Bell|206 JetRanger|H1T|L|H
B407|Bell|407|H1T|L|H
B412|Bell|412|H2T|L|H
B429|Bell|429|H2T|L|H
B212|Bell|212|H2T|L|H
B505|Bell|505 Jet Ranger X|H1T|L|H
UH1|Bell|UH-1 Iroquois|H1T|L|M
EC20|Airbus Helicopters|EC120 Colibri|H1T|L|H
EC30|Airbus Helicopters|EC130 / H130|H1T|L|H
EC35|Airbus Helicopters|EC135 / H135|H2T|L|H
EC45|Airbus Helicopters|EC145 / H145|H2T|L|H
EC55|Airbus Helicopters|EC155 / H155|H2T|L|H
EC75|Airbus Helicopters|H175|H2T|M|H
EC25|Airbus Helicopters|EC225 / H225 Super Puma|H2T|M|H
AS32|Airbus Helicopters|AS332 Super Puma|H2T|M|H
AS50|Airbus Helicopters|AS350 / H125 Ecureuil|H1T|L|H
AS55|Airbus Helicopters|AS355 Ecureuil 2|H2T|L|H
AS65|Airbus Helicopters|AS365 Dauphin|H2T|L|H
A109|Leonardo|AW109|H2T|L|H
A119|Leonardo|AW119 Koala|H1T|L|H
A139|Leonardo|AW139|H2T|L|H
A169|Leonardo|AW169|H2T|L|H
A189|Leonardo|AW189|H2T|M|H
EH10|Leonardo|AW101 Merlin|H3T|M|M
S76|Sikorsky|S-76|H2T|L|H
S92|Sikorsky|S-92|H2T|M|H
S61|Sikorsky|S-61 Sea King|H2T|M|H
H60|Sikorsky|H-60 Black Hawk / Seahawk|H2T|M|M
H53|Sikorsky|H-53 Sea Stallion|H2T|M|M
H47|Boeing|CH-47 Chinook|H2T|M|M
H64|Boeing|AH-64 Apache|H2T|M|M
NH90|NHIndustries|NH90|H2T|M|M
LYNX|Westland|Lynx|H2T|L|M
PUMA|Aerospatiale|SA 330 Puma|H2T|L|M
GAZL|Aerospatiale|SA 341 Gazelle|H1T|L|H
MI8|Mil|Mi-8 / Mi-17|H2T|M|H
MI26|Mil|Mi-26|H2T|M|H
KA27|Kamov|Ka-27 / Ka-32|H2T|M|H
H500|MD Helicopters|500 / Hughes 369|H1T|L|H
MD52|MD Helicopters|MD 520N|H1T|L|H
EXPL|MD Helicopters|MD 900 Explorer|H2T|L|H
V22|Bell Boeing|V-22 Osprey|T2T|M|M
C130|Lockheed|C-130 Hercules|L4T|M|M
C30J|Lockheed Martin|C-130J Super Hercules|L4T|M|M
C17|Boeing|C-17 Globemaster III|L4J|H|M
C5M|Lockheed|C-5M Super Galaxy|L4J|H|M
C160|Transall|C-160|L2T|M|M
C27J|Leonardo|C-27J Spartan|L2T|M|M
C295|Airbus|C295|L2T|M|M
CN35|Airbus|CN-235|L2T|M|M
K35R|Boeing|KC-135R Stratotanker|L4J|H|M
K35E|Boeing|KC-135E Stratotanker|L4J|H|M
R135|Boeing|RC-135|L4J|H|M
E3TF|Boeing|E-3 Sentry (TF33)|L4J|H|M
E3CF|Boeing|E-3 Sentry (CFM56)|L4J|H|M
E6|Boeing|E-6 Mercury|L4J|H|M
E737|Boeing|E-7 Wedgetail (737 AEW&C)|L2J|M|M
P8|Boeing|P-8 Poseidon|L2J|M|M
P3|Lockheed|P-3 Orion|L4T|M|M
C2|Grumman|C-2 Greyhound|L2T|M|M
E2|Northrop Grumman|E-2 Hawkeye|L2T|M|M
B52|Boeing|B-52 Stratofortress|L8J|H|M
B1|Rockwell|B-1 Lancer|L4J|H|M
B2|Northrop Grumman|B-2 Spirit|L4J|H|M
U2|Lockheed|U-2|L1J|M|M
F15|McDonnell Douglas|F-15 Eagle|L2J|M|M
F16|General Dynamics|F-16 Fighting Falcon|L1J|M|M
F18H|McDonnell Douglas|F/A-18 Hornet|L2J|M|M
F18S|Boeing|F/A-18E/F Super Hornet|L2J|M|M
F22|Lockheed Martin|F-22 Raptor|L2J|M|M
F35|Lockheed Martin|F-35 Lightning II|L1J|M|M
A10|Fairchild Republic|A-10 Thunderbolt II|L2J|M|M
EUFI|Eurofighter|Typhoon|L2J|M|M
TOR|Panavia|Tornado|L2J|M|M
RFAL|Dassault|Rafale|L2J|M|M
MIR2|Dassault|Mirage 2000|L1J|M|M
SB39|Saab|JAS 39 Gripen|L1J|M|M
HAWK|BAE Systems|Hawk|L1J|M|M
M346|Leonardo|M-346 Master|L2J|M|M
L39|Aero|L-39 Albatros|L1J|L|M
T38|Northrop|T-38 Talon|L2J|L|M
TEX2|Beechcraft|T-6 Texan II|L1T|L|M
PC21|Pilatus|PC-21|L1T|L|M
PC9|Pilatus|PC-9|L1T|L|M
PC7|Pilatus|PC-7|L1T|L|M
TUCA|Embraer|EMB-312 Tucano|L1T|L|M
Q4|Northrop Grumman|RQ-4 Global Hawk|L1J|M|U
Q9|General Atomics|MQ-9 Reaper|L1T|L|U
ZZZZ|(unknown)|Type without ICAO designator|-|-|X
`
