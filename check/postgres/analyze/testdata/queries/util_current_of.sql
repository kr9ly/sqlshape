UPDATE orders SET note = $1 WHERE CURRENT OF cur
