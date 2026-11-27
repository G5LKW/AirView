// Country name -> ISO 3166-1 alpha-2, generated from internal/refdata/country.go (the names the API returns in
// 'country' fields). The statistics API reports countries by name only; this lets the page draw flags.
const RAW = [
  `Afghanistan|AF;Albania|AL;Algeria|DZ;Andorra|AD;Angola|AO;Anguilla|AI;Antigua and Barbuda|AG;Argentina|AR;Armenia|AM;Aruba|AW;Australia|AU;Austria|AT;`,
  `Azerbaijan|AZ;Bahamas|BS;Bahrain|BH;Bangladesh|BD;Barbados|BB;Belarus|BY;Belgium|BE;Belize|BZ;Benin|BJ;Bermuda|BM;Bhutan|BT;Bolivia|BO;Bosnia and Herz`,
  `egovina|BA;Botswana|BW;Brazil|BR;British Virgin Islands|VG;Brunei|BN;Bulgaria|BG;Burkina Faso|BF;Burundi|BI;Cabo Verde|CV;Cambodia|KH;Cameroon|CM;Cana`,
  `da|CA;Cayman Islands|KY;Central African Republic|CF;Chad|TD;Chile|CL;China|CN;Colombia|CO;Comoros|KM;Congo|CG;Cook Islands|CK;Costa Rica|CR;Croatia|HR`,
  `;Cuba|CU;Curaçao|CW;Cyprus|CY;Czechia|CZ;Côte d'Ivoire|CI;DR Congo|CD;Denmark|DK;Djibouti|DJ;Dominican Republic|DO;Dominica|DM;Ecuador|EC;Egypt|EG;E`,
  `l Salvador|SV;Equatorial Guinea|GQ;Eritrea|ER;Estonia|EE;Eswatini|SZ;Ethiopia|ET;Falkland Islands|FK;Faroe Islands|FO;Fiji|FJ;Finland|FI;France|FR;Fre`,
  `nch Polynesia|PF;Gabon|GA;Gambia|GM;Georgia|GE;Germany|DE;Ghana|GH;Gibraltar|GI;Greece|GR;Greenland|GL;Grenada|GD;Guam|GU;Guatemala|GT;Guernsey|GG;Gui`,
  `nea-Bissau|GW;Guinea|GN;Guyana|GY;Haiti|HT;Honduras|HN;Hong Kong|HK;Hungary|HU;Iceland|IS;India|IN;Indonesia|ID;Iran|IR;Iraq|IQ;Ireland|IE;Isle of Man`,
  `|IM;Israel|IL;Italy|IT;Jamaica|JM;Japan|JP;Jersey|JE;Jordan|JO;Kazakhstan|KZ;Kenya|KE;Kiribati|KI;Kosovo|XK;Kuwait|KW;Kyrgyzstan|KG;Laos|LA;Latvia|LV;`,
  `Lebanon|LB;Lesotho|LS;Liberia|LR;Libya|LY;Liechtenstein|LI;Lithuania|LT;Luxembourg|LU;Macau|MO;Madagascar|MG;Malawi|MW;Malaysia|MY;Maldives|MV;Mali|ML`,
  `;Malta|MT;Marshall Islands|MH;Mauritania|MR;Mauritius|MU;Mexico|MX;Micronesia|FM;Moldova|MD;Monaco|MC;Mongolia|MN;Montenegro|ME;Montserrat|MS;Morocco|`,
  `MA;Mozambique|MZ;Myanmar|MM;Namibia|NA;Nauru|NR;Nepal|NP;Netherlands|NL;New Caledonia|NC;New Zealand|NZ;Nicaragua|NI;Nigeria|NG;Niger|NE;Niue|NU;North`,
  ` Korea|KP;North Macedonia|MK;Norway|NO;Oman|OM;Pakistan|PK;Palau|PW;Palestine|PS;Panama|PA;Papua New Guinea|PG;Paraguay|PY;Peru|PE;Philippines|PH;Pola`,
  `nd|PL;Portugal|PT;Puerto Rico|PR;Qatar|QA;Romania|RO;Russia|RU;Rwanda|RW;Réunion|RE;Saint Kitts and Nevis|KN;Saint Lucia|LC;Saint Vincent and the Gre`,
  `nadines|VC;Samoa|WS;San Marino|SM;Saudi Arabia|SA;Senegal|SN;Serbia|RS;Seychelles|SC;Sierra Leone|SL;Singapore|SG;Sint Maarten|SX;Slovakia|SK;Slovenia`,
  `|SI;Solomon Islands|SB;Somalia|SO;South Africa|ZA;South Korea|KR;South Sudan|SS;Spain|ES;Sri Lanka|LK;Sudan|SD;Suriname|SR;Sweden|SE;Switzerland|CH;Sy`,
  `ria|SY;São Tomé and Príncipe|ST;Taiwan|TW;Tajikistan|TJ;Tanzania|TZ;Thailand|TH;Timor-Leste|TL;Togo|TG;Tonga|TO;Trinidad and Tobago|TT;Tunisia|TN;T`,
  `urkey|TR;Turkmenistan|TM;Turks and Caicos Islands|TC;Tuvalu|TV;Uganda|UG;Ukraine|UA;United Arab Emirates|AE;United Kingdom|GB;United States|US;Uruguay`,
  `|UY;Uzbekistan|UZ;Vanuatu|VU;Venezuela|VE;Vietnam|VN;Yemen|YE;Zambia|ZM;Zimbabwe|ZW`,
].join('');

const byName = new Map();
for (const pair of RAW.split(';')) {
  const i = pair.lastIndexOf('|');
  if (i > 0) byName.set(pair.slice(0, i).toLowerCase(), pair.slice(i + 1));
}

/** ISO code for an English country name as used by the AirView API ('' when unknown). */
export function countryCode(name) {
  return name ? byName.get(String(name).toLowerCase()) || '' : '';
}

/** Teach the table a pair seen in live data (model.Aircraft.country / countryCode). */
export function learnCountry(name, cc) {
  if (name && cc && !byName.has(String(name).toLowerCase())) byName.set(String(name).toLowerCase(), cc);
}
