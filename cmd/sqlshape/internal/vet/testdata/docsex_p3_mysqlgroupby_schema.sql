-- sqlshape: mysql 8.4
-- mysql.md "Grouping": users, exactly as the example's two statements need.
CREATE TABLE users (
    id    BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    name  VARCHAR(100) NOT NULL,
    email VARCHAR(255) NOT NULL
);
