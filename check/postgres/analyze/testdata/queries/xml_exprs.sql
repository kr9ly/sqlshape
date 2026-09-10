SELECT xmlelement(name item, xmlattributes(id AS id, note AS n), total) AS el, xmlforest(id, note) AS fo, xmlconcat(xmlelement(name a), xmlelement(name b)) AS cc,
       xmlparse(content note) AS pc, xmlserialize(content xmlparse(content '<a/>') AS text) AS ser, xmlpi(name php, 'echo') AS pi,
       xmlroot(xmlparse(document '<a/>'), version '1.0', standalone yes) AS rt, xmlparse(content note) IS DOCUMENT AS isdoc, xmlagg(xmlelement(name n, id)) OVER () AS agg, xmlcomment(note) AS cm
FROM orders
