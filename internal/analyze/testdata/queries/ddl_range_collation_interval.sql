SELECT fr, upper(fr) AS hi, fr && $1 AS ov, mr, range_agg(fr) OVER () AS agg, label, label COLLATE mycoll AS lc, cd, every, lag3, d2s, every + lag3 AS sum
FROM spans
